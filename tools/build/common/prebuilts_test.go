package common

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// TestLoadPrebuiltsManifest_Real parses the actual tools/build/prebuilts.toml
// from the repo and verifies its structure matches expectations. This guards
// against schema drift when the manifest is edited.
func TestLoadPrebuiltsManifest_Real(t *testing.T) {
	root, err := FindRoot()
	if err != nil {
		t.Skipf("find root: %v", err)
	}
	m, err := LoadPrebuiltsManifest(root)
	if err != nil {
		t.Fatalf("LoadPrebuiltsManifest: %v", err)
	}
	if m.Schema != PrebuiltsManifestSchema {
		t.Errorf("schema = %d, want %d", m.Schema, PrebuiltsManifestSchema)
	}
	llvm := m.Binaries["llvm"]
	if llvm == nil {
		t.Fatal("manifest has no [binaries.llvm] entry")
	}
	if llvm.Version == "" {
		t.Error("llvm.version is empty")
	}
	wantTargets := []string{"linux-amd64", "darwin-arm64", "darwin-amd64", "windows-amd64"}
	for _, target := range wantTargets {
		te := llvm.Targets[target]
		if te == nil {
			t.Errorf("missing [binaries.llvm.targets.%s]", target)
			continue
		}
		if te.Unsupported != "" {
			// Placeholder targets (e.g., upstream stopped publishing macOS x86_64
			// tarballs at LLVM 22) are valid with empty url/files.
			continue
		}
		if te.URL == "" {
			t.Errorf("target %s: empty url", target)
		}
		if len(te.Files) == 0 {
			t.Errorf("target %s: no files declared", target)
		}
		// Every target must include opt and llc/lld outputs.
		var sawOpt bool
		for _, f := range te.Files {
			if strings.HasPrefix(f.Out, "opt") {
				sawOpt = true
			}
		}
		if !sawOpt {
			t.Errorf("target %s: no opt entry in files", target)
		}
	}
}

// TestPrebuiltsManifest_ValidateRejectsBadFiles ensures the validator catches
// FileOps with both src and glob set, or neither, and missing required fields.
func TestPrebuiltsManifest_ValidateRejectsBadFiles(t *testing.T) {
	cases := []struct {
		name string
		toml string
		want string
	}{
		{
			name: "missing src",
			toml: `schema = 1
[binaries.x]
version = "1"
bundle_dir = "out"
[binaries.x.targets.linux-amd64]
url = "https://example/a.tar.xz"
files = [{ out = "c.gz" }]
`,
			want: "missing src",
		},
		{
			name: "missing out",
			toml: `schema = 1
[binaries.x]
version = "1"
bundle_dir = "out"
[binaries.x.targets.linux-amd64]
url = "https://example/a.tar.xz"
files = [{ src = "a" }]
`,
			want: "missing out",
		},
		{
			name: "missing url",
			toml: `schema = 1
[binaries.x]
version = "1"
bundle_dir = "out"
[binaries.x.targets.linux-amd64]
sha256 = "deadbeef"
files = [{ src = "a", out = "b.gz" }]
`,
			want: "missing url",
		},
		{
			name: "wrong schema",
			toml: `schema = 99
[binaries.x]
version = "1"
bundle_dir = "out"
[binaries.x.targets.linux-amd64]
url = "https://example/a.tar.xz"
files = [{ src = "a", out = "b.gz" }]
`,
			want: "unsupported schema",
		},
		{
			name: "no binaries",
			toml: `schema = 1
`,
			want: "no [binaries.*] entries",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "prebuilts.toml")
			if err := os.WriteFile(path, []byte(tc.toml), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := loadPrebuiltsManifestFile(path)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

// TestBundleFromExtracted_MultipleFiles exercises the explicit file list against
// a staged extracted tree. The manifest is exhaustive (no globs, no
// auto-discovery), so multiple Src entries must round-trip correctly and any
// file not listed must be ignored.
func TestBundleFromExtracted_MultipleFiles(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	mkFile := func(rel, content string) {
		full := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mkFile("bin/opt", "OPT_BINARY")
	mkFile("lib/liblldCommon.dylib", "LLD_COMMON")
	mkFile("lib/liblldELF.dylib", "LLD_ELF")
	mkFile("lib/unrelated.txt", "should_not_match")

	files := []PrebuiltFile{
		{Src: "bin/opt", Out: "opt"},
		{Src: "lib/liblldCommon.dylib", Out: "liblldCommon.dylib"},
		{Src: "lib/liblldELF.dylib", Out: "liblldELF.dylib"},
	}
	if err := BundleFromExtracted(src, dst, files); err != nil {
		t.Fatalf("BundleFromExtracted: %v", err)
	}

	// `out` is the cache-relative name without .gz; bundling appends .gz.
	want := map[string]string{
		"opt.gz":                "OPT_BINARY",
		"liblldCommon.dylib.gz": "LLD_COMMON",
		"liblldELF.dylib.gz":    "LLD_ELF",
	}
	for name, wantContent := range want {
		got, err := readGzipped(filepath.Join(dst, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got != wantContent {
			t.Errorf("%s: got %q, want %q", name, got, wantContent)
		}
	}
	// Unlisted files must not appear in the bundle.
	if _, err := os.Stat(filepath.Join(dst, "unrelated.txt.gz")); !os.IsNotExist(err) {
		t.Errorf("unrelated.txt.gz should not exist (manifest is exhaustive)")
	}
}

// TestBundleFromExtracted_ResolveSymlink confirms that resolve_symlink walks
// the symlink chain (e.g., libLLVM.so → libLLVM.so.22 → libLLVM.so.22.1.0).
func TestBundleFromExtracted_ResolveSymlink(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	if err := os.MkdirAll(filepath.Join(src, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(src, "lib", "libLLVM.so.22")
	if err := os.WriteFile(real, []byte("REAL_LIB"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("libLLVM.so.22", filepath.Join(src, "lib", "libLLVM.so")); err != nil {
		t.Fatal(err)
	}

	files := []PrebuiltFile{
		{Src: "lib/libLLVM.so", Out: "libLLVM.so", ResolveSymlink: true},
	}
	if err := BundleFromExtracted(src, dst, files); err != nil {
		t.Fatalf("BundleFromExtracted: %v", err)
	}
	got, err := readGzipped(filepath.Join(dst, "libLLVM.so.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "REAL_LIB" {
		t.Errorf("got %q, want REAL_LIB", got)
	}
}

// TestFetchPrebuilt_HappyPath serves a small tar archive over HTTP, verifies
// SHA256, extracts, and confirms the cache marker is written.
func TestFetchPrebuilt_HappyPath(t *testing.T) {
	tarBytes, err := makeTarGzContent(map[string]string{
		"bin/opt": "OPT_CONTENT",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantHash := sha256Hex(tarBytes)

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(tarBytes)
	}))
	defer srv.Close()

	cacheRoot := t.TempDir()
	t.Setenv("PROMISE_PREBUILTS_CACHE", cacheRoot)

	manifestPath := filepath.Join(t.TempDir(), "prebuilts.toml")
	if err := os.WriteFile(manifestPath, []byte(`schema = 1
[binaries.demo]
version = "1.0.0"
bundle_dir = "out"
[binaries.demo.targets.linux-amd64]
url = "`+srv.URL+`/demo.tar.gz"
sha256 = "`+wantHash+`"
files = [{ src = "bin/opt", out = "opt" }]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := loadPrebuiltsManifestFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}

	cacheDir, err := FetchPrebuilt(m, "demo", "linux-amd64")
	if err != nil {
		t.Fatalf("FetchPrebuilt: %v", err)
	}

	// archive.ok and tools.ok markers exist.
	if !Exists(filepath.Join(cacheDir, "archive.ok")) {
		t.Errorf("missing archive.ok marker in %s", cacheDir)
	}
	if !Exists(filepath.Join(cacheDir, "tools.ok")) {
		t.Errorf("missing tools.ok marker in %s", cacheDir)
	}
	// Manifest's `out` file lives flat in the cache dir with the source content.
	got, err := os.ReadFile(filepath.Join(cacheDir, "opt"))
	if err != nil {
		t.Fatalf("read cached opt: %v", err)
	}
	if string(got) != "OPT_CONTENT" {
		t.Errorf("opt content = %q, want OPT_CONTENT", got)
	}

	// Second call must use the cache (no extra HTTP hit).
	if _, err := FetchPrebuilt(m, "demo", "linux-amd64"); err != nil {
		t.Fatalf("second FetchPrebuilt: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("HTTP hits = %d, want 1 (second call should be cached)", got)
	}
}

// TestFetchPrebuilt_BadSHA confirms that a hash mismatch is fatal and the
// success marker is NOT written.
func TestFetchPrebuilt_BadSHA(t *testing.T) {
	tarBytes, err := makeTarGzContent(map[string]string{"bin/opt": "X"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarBytes)
	}))
	defer srv.Close()

	cacheRoot := t.TempDir()
	t.Setenv("PROMISE_PREBUILTS_CACHE", cacheRoot)

	manifestPath := filepath.Join(t.TempDir(), "prebuilts.toml")
	if err := os.WriteFile(manifestPath, []byte(`schema = 1
[binaries.demo]
version = "1.0.0"
bundle_dir = "out"
[binaries.demo.targets.linux-amd64]
url = "`+srv.URL+`/demo.tar.gz"
sha256 = "0000000000000000000000000000000000000000000000000000000000000000"
files = [{ src = "bin/opt", out = "opt" }]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := loadPrebuiltsManifestFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}

	_, err = FetchPrebuilt(m, "demo", "linux-amd64")
	if err == nil {
		t.Fatal("expected sha256 mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Errorf("error = %v, want sha256 mismatch", err)
	}

	// Neither sentinel should be written when the hash mismatches.
	cacheDir := filepath.Join(cacheRoot, "demo", "1.0.0", "linux-amd64")
	for _, name := range []string{"archive.ok", "tools.ok"} {
		if Exists(filepath.Join(cacheDir, name)) {
			t.Errorf("%s exists despite mismatch: %s", name, filepath.Join(cacheDir, name))
		}
	}
}

// TestFetchPrebuilt_OptionalMissingTarget ensures optional binaries with no
// target entry return ("", nil) instead of an error.
func TestFetchPrebuilt_OptionalMissingTarget(t *testing.T) {
	manifestPath := filepath.Join(t.TempDir(), "prebuilts.toml")
	if err := os.WriteFile(manifestPath, []byte(`schema = 1
[binaries.demo]
version = "1.0.0"
bundle_dir = "out"
optional = true
[binaries.demo.targets.linux-amd64]
url = "https://example/a.tar.gz"
sha256 = "deadbeef"
files = [{ src = "a", out = "b.gz" }]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := loadPrebuiltsManifestFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	got, err := FetchPrebuilt(m, "demo", "darwin-arm64") // not in manifest
	if err != nil {
		t.Fatalf("expected nil error for optional missing target, got %v", err)
	}
	if got != "" {
		t.Errorf("expected empty path, got %q", got)
	}
}

// TestFetchPrebuilt_RequiredMissingTarget confirms required binaries with no
// target entry produce a clear error.
func TestFetchPrebuilt_RequiredMissingTarget(t *testing.T) {
	manifestPath := filepath.Join(t.TempDir(), "prebuilts.toml")
	if err := os.WriteFile(manifestPath, []byte(`schema = 1
[binaries.demo]
version = "1.0.0"
bundle_dir = "out"
[binaries.demo.targets.linux-amd64]
url = "https://example/a.tar.gz"
sha256 = "deadbeef"
files = [{ src = "a", out = "b.gz" }]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := loadPrebuiltsManifestFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = FetchPrebuilt(m, "demo", "darwin-arm64")
	if err == nil {
		t.Fatal("expected error for required missing target, got nil")
	}
	if !strings.Contains(err.Error(), "no target entry") {
		t.Errorf("error = %v, want 'no target entry'", err)
	}
}

// TestFetchAll_OnlySubset verifies the --fetch=name filter only fetches the
// named subset.
func TestFetchAll_OnlySubset(t *testing.T) {
	tarBytes, err := makeTarGzContent(map[string]string{"a": "A"})
	if err != nil {
		t.Fatal(err)
	}
	wantHash := sha256Hex(tarBytes)
	var aHits, bHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/a/") {
			atomic.AddInt32(&aHits, 1)
		} else {
			atomic.AddInt32(&bHits, 1)
		}
		_, _ = w.Write(tarBytes)
	}))
	defer srv.Close()

	t.Setenv("PROMISE_PREBUILTS_CACHE", t.TempDir())

	manifestPath := filepath.Join(t.TempDir(), "prebuilts.toml")
	if err := os.WriteFile(manifestPath, []byte(`schema = 1
[binaries.a]
version = "1"
bundle_dir = "outa"
[binaries.a.targets.linux-amd64]
url = "`+srv.URL+`/a/a.tar.gz"
sha256 = "`+wantHash+`"
files = [{ src = "a", out = "a" }]

[binaries.b]
version = "1"
bundle_dir = "outb"
[binaries.b.targets.linux-amd64]
url = "`+srv.URL+`/b/b.tar.gz"
sha256 = "`+wantHash+`"
files = [{ src = "a", out = "a" }]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := loadPrebuiltsManifestFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}

	out, err := FetchAll(m, "linux-amd64", []string{"a"})
	if err != nil {
		t.Fatalf("FetchAll: %v", err)
	}
	if _, ok := out["a"]; !ok {
		t.Error("expected a in result map")
	}
	if _, ok := out["b"]; ok {
		t.Error("b should be skipped (only=a)")
	}
	if got := atomic.LoadInt32(&aHits); got != 1 {
		t.Errorf("a hits = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&bHits); got != 0 {
		t.Errorf("b hits = %d, want 0 (filtered)", got)
	}
}

// TestFetchAll_UnknownInOnly returns an error when --fetch=foo names a binary
// that's not in the manifest.
func TestFetchAll_UnknownInOnly(t *testing.T) {
	manifestPath := filepath.Join(t.TempDir(), "prebuilts.toml")
	if err := os.WriteFile(manifestPath, []byte(`schema = 1
[binaries.a]
version = "1"
bundle_dir = "out"
[binaries.a.targets.linux-amd64]
url = "https://example/a.tar.gz"
sha256 = "deadbeef"
files = [{ src = "a", out = "a.gz" }]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := loadPrebuiltsManifestFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = FetchAll(m, "linux-amd64", []string{"unknown"})
	if err == nil {
		t.Fatal("expected error for unknown name in --fetch list")
	}
	if !strings.Contains(err.Error(), "not declared") {
		t.Errorf("error = %v, want 'not declared'", err)
	}
}

// TestCurrentBuildTarget verifies the target string format matches the
// existing on-disk resource directory naming.
func TestCurrentBuildTarget(t *testing.T) {
	got := CurrentBuildTarget()
	if !strings.Contains(got, "-") {
		t.Errorf("CurrentBuildTarget() = %q; want OS-arch form", got)
	}
}

// TestPrebuiltsManifest_ValidateRequiredFields covers the missing-version and
// missing-bundle_dir validation branches not exercised by the file-op cases.
func TestPrebuiltsManifest_ValidateRequiredFields(t *testing.T) {
	cases := []struct {
		name string
		toml string
		want string
	}{
		{
			name: "missing version",
			toml: `schema = 1
[binaries.x]
bundle_dir = "out"
[binaries.x.targets.linux-amd64]
url = "https://example/a.tar.xz"
files = [{ src = "a", out = "b.gz" }]
`,
			want: "missing version",
		},
		{
			name: "missing bundle_dir",
			toml: `schema = 1
[binaries.x]
version = "1"
[binaries.x.targets.linux-amd64]
url = "https://example/a.tar.xz"
files = [{ src = "a", out = "b.gz" }]
`,
			want: "missing bundle_dir",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "prebuilts.toml")
			if err := os.WriteFile(path, []byte(tc.toml), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := loadPrebuiltsManifestFile(path)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

// TestFetchPrebuilt_UnknownName confirms that calling FetchPrebuilt with a name
// not present in the manifest produces a clear error (FetchAll filters before
// calling, so this exercises the direct-call path).
func TestFetchPrebuilt_UnknownName(t *testing.T) {
	manifestPath := filepath.Join(t.TempDir(), "prebuilts.toml")
	if err := os.WriteFile(manifestPath, []byte(`schema = 1
[binaries.demo]
version = "1"
bundle_dir = "out"
[binaries.demo.targets.linux-amd64]
url = "https://example/a.tar.gz"
sha256 = "deadbeef"
files = [{ src = "a", out = "b.gz" }]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := loadPrebuiltsManifestFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = FetchPrebuilt(m, "nonexistent", "linux-amd64")
	if err == nil {
		t.Fatal("expected error for unknown binary name, got nil")
	}
	if !strings.Contains(err.Error(), "not declared") {
		t.Errorf("error = %v, want 'not declared'", err)
	}
}

// TestFetchPrebuilt_EmptySHA256Warning verifies that an empty sha256 in the
// manifest skips verification (with a warning) but still extracts and writes
// the success marker. This is the "first-pass dev workflow" path called out
// in the manifest comment.
func TestFetchPrebuilt_EmptySHA256Warning(t *testing.T) {
	tarBytes, err := makeTarGzContent(map[string]string{"bin/opt": "OPT"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarBytes)
	}))
	defer srv.Close()

	t.Setenv("PROMISE_PREBUILTS_CACHE", t.TempDir())

	manifestPath := filepath.Join(t.TempDir(), "prebuilts.toml")
	if err := os.WriteFile(manifestPath, []byte(`schema = 1
[binaries.demo]
version = "1"
bundle_dir = "out"
[binaries.demo.targets.linux-amd64]
url = "`+srv.URL+`/demo.tar.gz"
sha256 = ""
files = [{ src = "bin/opt", out = "opt" }]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := loadPrebuiltsManifestFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}

	cacheDir, err := FetchPrebuilt(m, "demo", "linux-amd64")
	if err != nil {
		t.Fatalf("FetchPrebuilt with empty sha256: %v", err)
	}
	if !Exists(filepath.Join(cacheDir, "archive.ok")) {
		t.Error("archive.ok marker missing despite empty-sha success path")
	}
	if !Exists(filepath.Join(cacheDir, "tools.ok")) {
		t.Error("tools.ok marker missing despite empty-sha success path")
	}
	got, err := os.ReadFile(filepath.Join(cacheDir, "opt"))
	if err != nil || string(got) != "OPT" {
		t.Errorf("cached content = %q (err %v), want OPT", got, err)
	}
}

// TestExtractZip_HappyPath builds a small zip with a nested directory and a
// regular file, then exercises the full extractZip flow.
func TestExtractZip_HappyPath(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "test.zip")
	if err := writeZipArchive(zipPath, map[string]string{
		"top.txt":         "TOP",
		"sub/inner.txt":   "INNER",
		"sub/deep/x.txt":  "DEEP",
		"empty/":          "", // directory entry with trailing slash
	}); err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	if err := ExtractArchive(zipPath, dst); err != nil {
		t.Fatalf("extractArchive: %v", err)
	}

	for path, want := range map[string]string{
		"top.txt":        "TOP",
		"sub/inner.txt":  "INNER",
		"sub/deep/x.txt": "DEEP",
	} {
		got, err := os.ReadFile(filepath.Join(dst, path))
		if err != nil {
			t.Errorf("read %s: %v", path, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	if info, err := os.Stat(filepath.Join(dst, "empty")); err != nil || !info.IsDir() {
		t.Errorf("empty/ should be extracted as a directory; stat err=%v", err)
	}
}

// TestExtractZip_PathTraversal verifies the security check that rejects zip
// entries trying to escape the destination directory. Without this check, a
// malicious archive could overwrite arbitrary host files (Zip Slip CVE class).
func TestExtractZip_PathTraversal(t *testing.T) {
	cases := []struct {
		name      string
		entryName string
	}{
		{"parent escape", "../etc/passwd"},
		{"deep parent escape", "sub/../../../../etc/passwd"},
		{"absolute unix", "/etc/passwd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			zipPath := filepath.Join(dir, "bad.zip")
			if err := writeZipArchive(zipPath, map[string]string{
				tc.entryName: "PWNED",
			}); err != nil {
				t.Fatal(err)
			}
			dst := t.TempDir()
			err := ExtractArchive(zipPath, dst)
			if err == nil {
				t.Fatalf("expected refusal for entry %q, got nil", tc.entryName)
			}
			if !strings.Contains(err.Error(), "escaping path") {
				t.Errorf("error = %v, want 'escaping path'", err)
			}
			// Confirm the malicious payload was NOT written outside dst.
			// (The check fires before any extract attempt for the offending
			// entry, but a file written in the parent dir would be unmistakable.)
			outside := filepath.Join(filepath.Dir(dst), "etc", "passwd")
			if Exists(outside) {
				t.Errorf("file written outside dst at %s — escape!", outside)
			}
		})
	}
}

// TestExtractArchive_UnsupportedFormat ensures unrecognized archive extensions
// are rejected with a clear error rather than silently misinterpreted.
func TestExtractArchive_UnsupportedFormat(t *testing.T) {
	dir := t.TempDir()
	bogus := filepath.Join(dir, "archive.7z")
	if err := os.WriteFile(bogus, []byte("not a real archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := ExtractArchive(bogus, t.TempDir())
	if err == nil {
		t.Fatal("expected error for unsupported format, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported archive format") {
		t.Errorf("error = %v, want 'unsupported archive format'", err)
	}
}

// TestSingleTopLevelDir covers the three cases: exactly one subdir (the LLVM
// tarball shape), multiple subdirs, and zero subdirs.
func TestSingleTopLevelDir(t *testing.T) {
	t.Run("one dir", func(t *testing.T) {
		root := t.TempDir()
		inner := filepath.Join(root, "clang+llvm-22.1.0")
		if err := os.MkdirAll(filepath.Join(inner, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := singleTopLevelDir(root)
		if err != nil {
			t.Fatal(err)
		}
		if got != inner {
			t.Errorf("got %s, want %s", got, inner)
		}
	})
	t.Run("multiple dirs falls back to root", func(t *testing.T) {
		root := t.TempDir()
		for _, d := range []string{"a", "b"} {
			if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		got, err := singleTopLevelDir(root)
		if err != nil {
			t.Fatal(err)
		}
		if got != root {
			t.Errorf("got %s, want %s (fallback when not exactly one dir)", got, root)
		}
	})
	t.Run("zero dirs falls back to root", func(t *testing.T) {
		root := t.TempDir()
		// Add a regular file but no directories.
		if err := os.WriteFile(filepath.Join(root, "f.txt"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := singleTopLevelDir(root)
		if err != nil {
			t.Fatal(err)
		}
		if got != root {
			t.Errorf("got %s, want %s (fallback when no dirs)", got, root)
		}
	})
}

// TestBundleLLVM_HappyPath wires the release-build bundling end-to-end against
// a synthetic flat cache dir (no actual download). After FetchPrebuilt the
// cache dir holds files at their `out` names; BundleLLVM gzips them into the
// embed dir as "<out>.gz".
func TestBundleLLVM_HappyPath(t *testing.T) {
	// Synthetic prebuilts cache dir with the manifest's `out` files flat.
	cacheDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cacheDir, "opt"), []byte("OPT_FETCHED"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "llc"), []byte("LLC_FETCHED"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Synthetic "repo root" + manifest declaring just the running target.
	root := t.TempDir()
	manifestDir := filepath.Join(root, "tools", "build")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := CurrentBuildTarget()
	manifestText := `schema = 1
[binaries.llvm]
version = "22.1.0"
bundle_dir = "compiler/cmd/promise/resources/llvm"
[binaries.llvm.targets.` + target + `]
url = "https://example/llvm.tar.xz"
sha256 = "deadbeef"
files = [
  { src = "bin/opt", out = "opt" },
  { src = "bin/llc", out = "llc" },
]
`
	if err := os.WriteFile(filepath.Join(manifestDir, "prebuilts.toml"), []byte(manifestText), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadPrebuiltsManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := BundleLLVM(root, m, cacheDir); err != nil {
		t.Fatalf("BundleLLVM: %v", err)
	}
	dst := filepath.Join(root, "compiler/cmd/promise/resources/llvm", target)
	for name, want := range map[string]string{
		"opt.gz": "OPT_FETCHED",
		"llc.gz": "LLC_FETCHED",
	} {
		got, err := readGzipped(filepath.Join(dst, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// TestBundleLLVM_NoCacheDir is the "all-optional, none fetched" short-circuit:
// returns nil without touching the filesystem.
func TestBundleLLVM_NoCacheDir(t *testing.T) {
	if err := BundleLLVM(t.TempDir(), &PrebuiltsManifest{}, ""); err != nil {
		t.Errorf("expected nil for empty cacheDir, got %v", err)
	}
}

// --- helpers ---

// makeTarGzContent builds a .tar.gz blob with the given file contents. Used by
// the fetch tests because gzip is universally supported by every host's `tar`,
// so the production extractArchive path runs unchanged in tests.
func makeTarGzContent(files map[string]string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(content)),
		}); err != nil {
			return nil, err
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// writeZipArchive builds a .zip file containing the given entries. Names ending
// in "/" become directory entries; all others become regular files. Used by the
// extractZip tests because the production extractZip path is the only place
// that ever has to consume a zip archive.
func writeZipArchive(path string, entries map[string]string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for name, content := range entries {
		if strings.HasSuffix(name, "/") {
			if _, err := zw.CreateHeader(&zip.FileHeader{
				Name: name,
			}); err != nil {
				return err
			}
			continue
		}
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		if _, err := w.Write([]byte(content)); err != nil {
			return err
		}
	}
	return zw.Close()
}

// readGzipped reads a gzip-compressed file and returns the decompressed contents.
func readGzipped(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	out, err := io.ReadAll(gz)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
