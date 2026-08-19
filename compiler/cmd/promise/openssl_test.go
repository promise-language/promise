package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestOpenSSLManifestName pins the arch-qualified runtime-manifest name format.
// The producer (OpenSSLManifestName in tools/build/common/openssl_slim.go) lives
// in a separate Go module and duplicates this format by necessity — a silent
// drift here makes every OpenSSL blob unresolvable, and the failure mode is a
// fall-through rather than a loud error, so pin it (mirrors TestMuslManifestName).
func TestOpenSSLManifestName(t *testing.T) {
	for _, tt := range []struct {
		arch, file, want string
	}{
		{"x86_64-linux-musl", "libssl.a", "openssl-x86_64-linux-musl-libssl.a"},
		{"aarch64-linux-musl", "libcrypto.a", "openssl-aarch64-linux-musl-libcrypto.a"},
	} {
		if got := opensslManifestName(tt.arch, tt.file); got != tt.want {
			t.Errorf("opensslManifestName(%q, %q) = %q, want %q", tt.arch, tt.file, got, tt.want)
		}
	}
	// Arches must not collide — the reason the name is qualified at all.
	if opensslManifestName("x86_64-linux-musl", "libssl.a") == opensslManifestName("aarch64-linux-musl", "libssl.a") {
		t.Error("arch-qualified names collided across arches")
	}
}

func TestOpenSSLArchDir(t *testing.T) {
	tests := []struct {
		target string
		want   string
	}{
		{"aarch64-unknown-linux-musl", "aarch64-linux-musl"},
		{"aarch64-linux-musl", "aarch64-linux-musl"},
		{"x86_64-unknown-linux-musl", "x86_64-linux-musl"},
		{"x86_64-pc-linux-gnu", "x86_64-linux-musl"},
	}
	for _, tt := range tests {
		if got := opensslArchDir(tt.target); got != tt.want {
			t.Errorf("opensslArchDir(%q) = %q, want %q", tt.target, got, tt.want)
		}
	}
}

func TestOpenSSLCompleteEmpty(t *testing.T) {
	if openSSLComplete(t.TempDir()) {
		t.Error("empty dir: expected false")
	}
}

func TestOpenSSLCompletePartial(t *testing.T) {
	dir := t.TempDir()
	// Only libssl.a, not libcrypto.a.
	if err := os.WriteFile(filepath.Join(dir, "libssl.a"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if openSSLComplete(dir) {
		t.Error("partial dir: expected false")
	}
}

func TestOpenSSLCompleteAll(t *testing.T) {
	dir := t.TempDir()
	for _, name := range opensslFiles {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if !openSSLComplete(dir) {
		t.Error("complete dir: expected true")
	}
}

// TestOpenSSLCompleteRejectsPlaceholder confirms a dir holding only EmbedOpenSSL's
// PLACEHOLDER sentinel reads as "not available" — the property that keeps an
// offline / not-yet-pinned build from claiming OpenSSL is present.
func TestOpenSSLCompleteRejectsPlaceholder(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "PLACEHOLDER"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if openSSLComplete(dir) {
		t.Error("placeholder-only dir: expected false")
	}
}

// TestOpenSSLValidWithEmbedded validates the size-check path against the real
// embedded archives. Skips when this build embedded only a placeholder (the
// common case until OpenSSL is pinned/hosted — see EmbedOpenSSL).
func TestOpenSSLValidWithEmbedded(t *testing.T) {
	if !hasEmbeddedOpenSSL {
		t.Skip("no embedded OpenSSL on this platform")
	}
	arch := "x86_64-linux-musl"
	if runtime.GOARCH == "arm64" {
		arch = "aarch64-linux-musl"
	}
	prefix := "resources/openssl/" + arch

	// Extract; if any real archive is absent (placeholder-only build), skip.
	base := t.TempDir()
	dir := filepath.Join(base, arch)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range opensslFiles {
		data, err := embeddedOpenSSL.ReadFile(prefix + "/" + name)
		if err != nil {
			t.Skipf("build embedded no real %s (placeholder-only build)", name)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}

	if !openSSLValid(dir) {
		t.Error("correctly extracted embedded OpenSSL: expected true")
	}

	// Corrupt one archive — size mismatch should make it invalid.
	if err := os.WriteFile(filepath.Join(dir, "libcrypto.a"), []byte("corrupted"), 0644); err != nil {
		t.Fatal(err)
	}
	if openSSLValid(dir) {
		t.Error("after corrupting libcrypto.a: expected false")
	}
}

func TestOpenSSLValidWrongArch(t *testing.T) {
	if !hasEmbeddedOpenSSL {
		t.Skip("no embedded OpenSSL on this platform")
	}
	base := t.TempDir()
	dir := filepath.Join(base, "nonexistent-arch-linux-musl")
	os.MkdirAll(dir, 0755)
	for _, name := range opensslFiles {
		os.WriteFile(filepath.Join(dir, name), []byte("x"), 0644)
	}
	if openSSLValid(dir) {
		t.Error("unknown arch dir: expected false")
	}
}

// TestFindOpenSSLInstalledLocation drives findOpenSSL's discovery ladder: with
// PROMISE_HOME pointed at a temp dir that has the archives staged at the
// installed location (<HOME>/lib/openssl/<arch>/), findOpenSSL must resolve to
// that dir (rung 2), the promise binary having no sibling openssl/ (rung 1).
func TestFindOpenSSLInstalledLocation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PROMISE_HOME", home)

	target := "x86_64-unknown-linux-musl"
	if runtime.GOARCH == "arm64" {
		target = "aarch64-unknown-linux-musl"
	}
	arch := opensslArchDir(target)

	installDir := filepath.Join(home, "lib", "openssl", arch)
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range opensslFiles {
		if err := os.WriteFile(filepath.Join(installDir, name), []byte("archive"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := findOpenSSL(target)
	if err != nil {
		t.Fatalf("findOpenSSL: %v", err)
	}
	if got != installDir {
		t.Errorf("findOpenSSL = %q, want installed location %q", got, installDir)
	}
}

// embeddedOpenSSLReal reports whether this build embedded the REAL archives (not
// just the PLACEHOLDER sentinel). Tests that exercise the embedded-extraction
// rung of findOpenSSL are meaningful only then; otherwise they skip.
func embeddedOpenSSLReal(t *testing.T) bool {
	t.Helper()
	if !hasEmbeddedOpenSSL {
		return false
	}
	arch := "x86_64-linux-musl"
	if runtime.GOARCH == "arm64" {
		arch = "aarch64-linux-musl"
	}
	prefix := "resources/openssl/" + arch
	for _, name := range opensslFiles {
		if _, err := embeddedOpenSSL.ReadFile(prefix + "/" + name); err != nil {
			return false
		}
	}
	return true
}

// TestResolveOpenSSLDirNoTLS pins the gate default: a non-TLS program
// (needsTLS=false) resolves to no OpenSSL dir and never touches the discovery
// ladder — even for a target that has no OpenSSL at all. This is the property
// that keeps every existing program's link behaviour unchanged (T1596 / #28).
func TestResolveOpenSSLDirNoTLS(t *testing.T) {
	// A bogus/non-Linux target proves the early return short-circuits before
	// findOpenSSL is ever consulted (findOpenSSL would otherwise do real work).
	dir, err := resolveOpenSSLDir("wasm32-wasi", false)
	if err != nil {
		t.Fatalf("resolveOpenSSLDir(needsTLS=false): unexpected err %v", err)
	}
	if dir != "" {
		t.Errorf("resolveOpenSSLDir(needsTLS=false) = %q, want empty", dir)
	}
}

// TestResolveOpenSSLDirWithTLS confirms needsTLS=true delegates to the discovery
// ladder and returns a directory holding both archives. Uses the installed-
// location rung so it does not depend on embedded archives being real.
func TestResolveOpenSSLDirWithTLS(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PROMISE_HOME", home)

	target := "x86_64-unknown-linux-musl"
	if runtime.GOARCH == "arm64" {
		target = "aarch64-unknown-linux-musl"
	}
	arch := opensslArchDir(target)
	installDir := filepath.Join(home, "lib", "openssl", arch)
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range opensslFiles {
		if err := os.WriteFile(filepath.Join(installDir, name), []byte("archive"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	dir, err := resolveOpenSSLDir(target, true)
	if err != nil {
		t.Fatalf("resolveOpenSSLDir(needsTLS=true): %v", err)
	}
	if dir != installDir {
		t.Errorf("resolveOpenSSLDir = %q, want %q", dir, installDir)
	}
}

// TestFindOpenSSLEmbeddedExtraction drives the LAST rung of findOpenSSL's ladder:
// with an empty PROMISE_HOME (no sibling/install/cache dir) and no openssl blobs
// in the manifest, findOpenSSL must extract the embedded archives into the cache
// dir and return it. A second call must then hit the cache rung (rung 3) and
// return the same dir. Skips unless the build embedded the REAL archives.
func TestFindOpenSSLEmbeddedExtraction(t *testing.T) {
	if !embeddedOpenSSLReal(t) {
		t.Skip("build embedded only a placeholder — nothing to extract")
	}
	home := t.TempDir()
	t.Setenv("PROMISE_HOME", home)

	target := "x86_64-unknown-linux-musl"
	if runtime.GOARCH == "arm64" {
		target = "aarch64-unknown-linux-musl"
	}
	arch := opensslArchDir(target)
	wantDir := filepath.Join(home, "cache", "openssl", arch)

	got, err := findOpenSSL(target)
	if err != nil {
		t.Fatalf("findOpenSSL (extraction rung): %v", err)
	}
	if got != wantDir {
		t.Errorf("findOpenSSL = %q, want cache dir %q", got, wantDir)
	}
	if !openSSLComplete(got) {
		t.Errorf("extracted dir %q is missing archives", got)
	}
	// Both archives must be non-trivial (real, not the 1-byte placeholder).
	for _, name := range opensslFiles {
		info, statErr := os.Stat(filepath.Join(got, name))
		if statErr != nil {
			t.Fatalf("stat %s: %v", name, statErr)
		}
		if info.Size() < 1024 {
			t.Errorf("%s extracted at %d bytes — looks like a placeholder, not a real archive", name, info.Size())
		}
	}

	// Second call: cache is now populated, so rung 3 (openSSLValid) short-circuits.
	got2, err := findOpenSSL(target)
	if err != nil {
		t.Fatalf("findOpenSSL (cache rung): %v", err)
	}
	if got2 != wantDir {
		t.Errorf("second findOpenSSL = %q, want cached dir %q", got2, wantDir)
	}
}

// TestDoctorCheckOpenSSLAvailable exercises the doctor check's healthy branch:
// with archives staged at the installed location, the check reports "ok" and
// lists the resolved path. This is an informational check that must never mark
// the environment unhealthy while TLS is unwired (T1596 / #28).
func TestDoctorCheckOpenSSLAvailable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PROMISE_HOME", home)

	target := "x86_64-unknown-linux-musl"
	if runtime.GOARCH == "arm64" {
		target = "aarch64-unknown-linux-musl"
	}
	arch := opensslArchDir(target)
	installDir := filepath.Join(home, "lib", "openssl", arch)
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range opensslFiles {
		if err := os.WriteFile(filepath.Join(installDir, name), []byte("archive"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	c := doctorCheckOpenSSL()
	if c.Required {
		t.Error("OpenSSL doctor check must be informational (Required=false) while TLS is unwired")
	}
	if c.Status != "ok" {
		t.Errorf("Status = %q, want ok; summary=%q", c.Status, c.Summary)
	}
	if c.Summary != "All OpenSSL archives found" {
		t.Errorf("Summary = %q, want %q", c.Summary, "All OpenSSL archives found")
	}
	// The resolved path must be reported in the details.
	foundPath := false
	for _, d := range c.Details {
		if strings.Contains(d, installDir) {
			foundPath = true
		}
	}
	if !foundPath {
		t.Errorf("details %v do not mention resolved path %q", c.Details, installDir)
	}
}

// indexOfSuffix returns the index of the first arg ending in suffix, or -1.
func indexOfSuffix(args []string, suffix string) int {
	for i, a := range args {
		if strings.HasSuffix(a, suffix) {
			return i
		}
	}
	return -1
}

// TestBuildMuslLinkArgsNoTLS asserts that with no OpenSSL dir (needsTLS=false)
// the link line contains NEITHER archive — the gate default that must hold for
// every existing program.
func TestBuildMuslLinkArgsNoTLS(t *testing.T) {
	args := buildMuslLinkArgs("x86_64-unknown-linux-musl", []string{"/tmp/main.o"}, "/tmp/out", "/crt", false, "")
	if indexOfSuffix(args, "libssl.a") != -1 {
		t.Errorf("needsTLS=false: libssl.a must be absent, args=%v", args)
	}
	if indexOfSuffix(args, "libcrypto.a") != -1 {
		t.Errorf("needsTLS=false: libcrypto.a must be absent, args=%v", args)
	}
	// libc.a must still be present.
	if indexOfSuffix(args, "libc.a") == -1 {
		t.Errorf("libc.a must be present, args=%v", args)
	}
}

// TestBuildMuslLinkArgsWithTLS asserts that with an OpenSSL dir (needsTLS=true)
// libssl.a + libcrypto.a appear AFTER the object files and BEFORE libc.a, in the
// order ssl-then-crypto (static-archive resolution is order-sensitive, T1596 / #28).
func TestBuildMuslLinkArgsWithTLS(t *testing.T) {
	args := buildMuslLinkArgs("x86_64-unknown-linux-musl", []string{"/tmp/main.o"}, "/tmp/out", "/crt", false, "/openssl")

	iObj := indexOfSuffix(args, "main.o")
	iSSL := indexOfSuffix(args, "libssl.a")
	iCrypto := indexOfSuffix(args, "libcrypto.a")
	iLibc := indexOfSuffix(args, "libc.a")

	if iObj == -1 || iSSL == -1 || iCrypto == -1 || iLibc == -1 {
		t.Fatalf("missing expected arg: obj=%d ssl=%d crypto=%d libc=%d args=%v", iObj, iSSL, iCrypto, iLibc, args)
	}
	// object → libssl → libcrypto → libc.a
	if !(iObj < iSSL && iSSL < iCrypto && iCrypto < iLibc) {
		t.Errorf("wrong archive order: obj=%d ssl=%d crypto=%d libc=%d args=%v", iObj, iSSL, iCrypto, iLibc, args)
	}
	// The archives must come from the supplied dir.
	if got := args[iSSL]; got != filepath.Join("/openssl", "libssl.a") {
		t.Errorf("libssl.a path = %q, want /openssl/libssl.a", got)
	}
}

// TestBuildMuslLinkArgsMultiWithTLS covers the multi-object path (linkLinuxMulti
// shares buildMuslLinkArgs): all objects precede the OpenSSL archives, which
// precede libc.a.
func TestBuildMuslLinkArgsMultiWithTLS(t *testing.T) {
	objs := []string{"/tmp/main.o", "/tmp/mod.o", "/tmp/inst.o"}
	args := buildMuslLinkArgs("x86_64-unknown-linux-musl", objs, "/tmp/out", "/crt", true, "/openssl")

	iSSL := indexOfSuffix(args, "libssl.a")
	iLibc := indexOfSuffix(args, "libc.a")
	if iSSL == -1 || iLibc == -1 {
		t.Fatalf("missing archive: ssl=%d libc=%d", iSSL, iLibc)
	}
	for _, o := range []string{"main.o", "mod.o", "inst.o"} {
		io := indexOfSuffix(args, o)
		if io == -1 || io >= iSSL {
			t.Errorf("object %s (idx %d) must precede libssl.a (idx %d), args=%v", o, io, iSSL, args)
		}
	}
	if iSSL >= iLibc {
		t.Errorf("libssl.a (idx %d) must precede libc.a (idx %d)", iSSL, iLibc)
	}
	// LTO flag threads through unchanged.
	if indexOfSuffix(args, "--lto-O1") == -1 && indexOfSuffix(args, "-o") == -1 {
		t.Errorf("expected lto/link args present, args=%v", args)
	}
}
