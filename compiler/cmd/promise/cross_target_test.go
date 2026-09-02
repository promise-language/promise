package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen"
)

// TestSupportedTargetsIsDeterministic pins that the advertised target set is a
// property of the compiler, not of the machine's cache state. An earlier draft
// probed the disk for musl CRT / compiler-rt payloads, which made
// `promise targets` answer differently on a warm and a cold cache — the same
// class of host-dependent behaviour that docs/distribution.md §1.1/§4 forbids
// (a cold cache means "not fetched yet", never "unsupported").
func TestSupportedTargetsIsDeterministic(t *testing.T) {
	// No t.Parallel: this test uses t.Setenv.

	// The exact set, not just "stable across two calls". Comparing two calls
	// alone would still pass on a host where a restored disk probe finds
	// nothing either time — it has to fail wherever the payloads do exist.
	want := []string{codegen.HostTargetTriple(), "wasm32-wasi", "wasm32-web"}
	got := make([]string, 0, len(want))
	for _, ts := range supportedTargets() {
		got = append(got, ts.Triple)
	}
	if !slices.Equal(got, want) {
		t.Errorf("supportedTargets() = %v, want exactly %v\n"+
			"a cross-native target may only appear here once its link payload ships (T0530/T0531/T0532), "+
			"and then as a static entry — never because one was found on disk", got, want)
	}

	// PROMISE_HOME is where a disk probe would look for cached payloads.
	// Repointing it at an empty directory must not change the answer.
	first := supportedTargets()
	t.Setenv("PROMISE_HOME", t.TempDir())
	second := supportedTargets()
	if !slices.Equal(first, second) {
		t.Errorf("supportedTargets() changed with PROMISE_HOME: %+v then %+v", first, second)
	}
}

// TestKnownTargetsSupersetOfSupported: anything this release can link, it must
// also be able to emit IR for. The reverse does not hold — cross targets are
// emittable long before their link payloads ship.
func TestKnownTargetsSupersetOfSupported(t *testing.T) {
	t.Parallel()
	known := make(map[string]bool)
	for _, ts := range knownTargets() {
		known[ts.Triple] = true
	}
	for _, ts := range supportedTargets() {
		if !known[ts.Triple] {
			t.Errorf("supported target %q is not in knownTargets()", ts.Triple)
		}
	}
	// The cross targets that cannot be linked yet must still be known, so
	// `emit-ir -target ...` works for them (T0533 Part 2).
	for _, triple := range []string{
		"x86_64-unknown-linux-musl", "aarch64-unknown-linux-musl",
		"x86_64-pc-windows-msvc", "aarch64-pc-windows-msvc",
	} {
		if !known[triple] {
			t.Errorf("cross target %q missing from knownTargets()", triple)
		}
	}
}

// TestKnownTargetsMarksHostNative guards the version-suffix path: the host
// triple carries an OS version the static table cannot enumerate, so it has to
// be appended rather than matched.
func TestKnownTargetsMarksHostNative(t *testing.T) {
	t.Parallel()
	natives := 0
	seen := make(map[string]bool)
	for _, ts := range knownTargets() {
		if seen[ts.Triple] {
			t.Errorf("duplicate triple %q in knownTargets()", ts.Triple)
		}
		seen[ts.Triple] = true
		if ts.Native {
			natives++
		}
		if ts.Display == "" {
			t.Errorf("knownTargets() entry %q has no display name", ts.Triple)
		}
	}
	if natives != 1 {
		t.Errorf("knownTargets() marked %d entries native, want exactly 1", natives)
	}
}

// TestUnlinkableTargetErrorIsAccurate: a real triple whose payload does not
// ship must not be reported as "invalid" — that sends the reader hunting for a
// typo. It must say linking is unavailable and point at emit-ir.
func TestUnlinkableTargetErrorIsAccurate(t *testing.T) {
	t.Parallel()
	msg := invalidTargetMessage("x86_64-pc-windows-msvc")
	for _, want := range []string{"cannot be built by this release", "known target", "emit-ir"} {
		if !strings.Contains(msg, want) {
			t.Errorf("invalidTargetMessage for a known-but-unlinkable target missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "invalid target") {
		t.Errorf("known target reported as invalid:\n%s", msg)
	}

	bogus := invalidTargetMessage("not-a-real-triple")
	if !strings.Contains(bogus, "invalid target") {
		t.Errorf("unknown target should be reported as invalid:\n%s", bogus)
	}
}

func TestSupportedTargetsAlwaysIncludesHostAndWasm(t *testing.T) {
	t.Parallel()
	targets := supportedTargets()
	hasNative := false
	hasWasi := false
	hasWeb := false
	for _, ts := range targets {
		if ts.Native {
			hasNative = true
		}
		if ts.Triple == "wasm32-wasi" {
			hasWasi = true
		}
		if ts.Triple == "wasm32-web" {
			hasWeb = true
		}
	}
	if !hasNative {
		t.Error("supportedTargets missing native host target")
	}
	if !hasWasi {
		t.Error("supportedTargets missing wasm32-wasi")
	}
	if !hasWeb {
		t.Error("supportedTargets missing wasm32-web")
	}
}

func TestSupportedTargetsCrossTargetsAreNotNative(t *testing.T) {
	t.Parallel()
	targets := supportedTargets()
	nativeCount := 0
	for _, ts := range targets {
		if ts.Native {
			nativeCount++
		}
	}
	if nativeCount != 1 {
		t.Errorf("expected exactly 1 native target, got %d", nativeCount)
	}
}

func TestHostShortNameCrossTargets(t *testing.T) {
	t.Parallel()
	cases := []struct {
		triple string
		want   string
	}{
		{"x86_64-unknown-linux-musl", "linux-x86_64"},
		{"aarch64-unknown-linux-musl", "linux-arm64"},
		{"x86_64-pc-windows-msvc", "windows-x86_64"},
		{"arm64-apple-macosx15.0.0", "darwin-arm64"},
	}
	for _, c := range cases {
		if got := hostShortName(c.triple); got != c.want {
			t.Errorf("hostShortName(%q) = %q, want %q", c.triple, got, c.want)
		}
	}
}

func TestDepFilesPresentWithRealFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	files := []string{"a.o", "b.o"}
	// Before creating files, depFilesPresent should return false.
	if depFilesPresent(dir, files) {
		t.Fatal("depFilesPresent true before files exist")
	}
	// Create only one file — still false.
	if err := os.WriteFile(filepath.Join(dir, "a.o"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if depFilesPresent(dir, files) {
		t.Fatal("depFilesPresent true with only one of two files")
	}
	// Create the second — now true.
	if err := os.WriteFile(filepath.Join(dir, "b.o"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !depFilesPresent(dir, files) {
		t.Fatal("depFilesPresent false with both files present")
	}
}

func TestDepFilesPresentEmptyList(t *testing.T) {
	t.Parallel()
	// An empty file list should return true — vacuously all present.
	dir := t.TempDir()
	if !depFilesPresent(dir, nil) {
		t.Fatal("depFilesPresent false for empty file list")
	}
}

func TestWriteTargetsText(t *testing.T) {
	t.Parallel()
	specs := []targetSpec{
		{Triple: "x86_64-unknown-linux-musl", Display: "linux-x86_64", Description: "Linux host", Native: true},
		{Triple: "wasm32-wasi", Display: "wasm32-wasi", Description: "WASM target"},
	}
	var buf bytes.Buffer
	writeTargets(&buf, specs, false)
	out := buf.String()
	if !strings.Contains(out, "Supported compile targets") {
		t.Errorf("missing header in text output:\n%s", out)
	}
	if !strings.Contains(out, "(native)") {
		t.Errorf("missing (native) marker:\n%s", out)
	}
	if !strings.Contains(out, "wasm32-wasi") {
		t.Errorf("missing wasm target:\n%s", out)
	}
	if !strings.Contains(out, "promise build -target") {
		t.Errorf("missing usage hint:\n%s", out)
	}
}

func TestWriteTargetsJSON(t *testing.T) {
	t.Parallel()
	specs := []targetSpec{
		{Triple: "x86_64-unknown-linux-musl", Display: "linux-x86_64", Description: "Linux host", Native: true},
		{Triple: "wasm32-wasi", Display: "wasm32-wasi", Description: "WASM target"},
	}
	var buf bytes.Buffer
	writeTargets(&buf, specs, true)
	var parsed struct {
		Host    string       `json:"host"`
		Targets []targetSpec `json:"targets"`
	}
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("invalid JSON output: %v\n%s", err, buf.String())
	}
	if len(parsed.Targets) != 2 {
		t.Errorf("expected 2 targets, got %d", len(parsed.Targets))
	}
	if parsed.Targets[0].Triple != "x86_64-unknown-linux-musl" {
		t.Errorf("unexpected first target triple: %s", parsed.Targets[0].Triple)
	}
}

func TestPrintTargetsUsage(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	printTargetsUsage(&buf)
	if got := buf.String(); !strings.Contains(got, "usage: promise targets") {
		t.Errorf("unexpected usage output: %s", got)
	}
}

func TestHostShortNameEdgeCases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		triple string
		want   string
	}{
		// Unknown OS — returned unchanged.
		{"riscv64-unknown-freebsd", "riscv64-unknown-freebsd"},
		// Known OS, unknown arch — OS without arch suffix.
		{"mips-unknown-linux-musl", "linux"},
		{"unknown-apple-macosx15.0.0", "darwin"},
		{"i686-pc-windows-msvc", "windows"},
		// darwin alias.
		{"aarch64-apple-darwin", "darwin-arm64"},
	}
	for _, c := range cases {
		if got := hostShortName(c.triple); got != c.want {
			t.Errorf("hostShortName(%q) = %q, want %q", c.triple, got, c.want)
		}
	}
}

func TestIsDarwinTarget(t *testing.T) {
	t.Parallel()
	if !isDarwinTarget("arm64-apple-macosx15.0.0") {
		t.Error("expected true for macOS triple")
	}
	if isDarwinTarget("x86_64-unknown-linux-musl") {
		t.Error("expected false for Linux triple")
	}
	if isDarwinTarget("") {
		t.Error("expected false for empty string")
	}
}

func TestIsWindowsTarget(t *testing.T) {
	t.Parallel()
	if !isWindowsTarget("x86_64-pc-windows-msvc") {
		t.Error("expected true for Windows triple")
	}
	if isWindowsTarget("x86_64-unknown-linux-musl") {
		t.Error("expected false for Linux triple")
	}
}

func TestBinaryExtension(t *testing.T) {
	t.Parallel()
	cases := []struct {
		target string
		want   string
	}{
		{"wasm32-wasi", ".wasm"},
		{"wasm32-web", ".wasm"},
		{"x86_64-pc-windows-msvc", ".exe"},
		{"x86_64-unknown-linux-musl", ""},
		{"arm64-apple-macosx15.0.0", ""},
	}
	for _, c := range cases {
		if got := binaryExtension(c.target); got != c.want {
			t.Errorf("binaryExtension(%q) = %q, want %q", c.target, got, c.want)
		}
	}
}
