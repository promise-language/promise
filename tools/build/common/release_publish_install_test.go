package common

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestInstallAssetName covers all (os, arch, variant) combos and the windows
// `.exe` insertion, locking the names to what scripts/install.sh + install.ps1
// compute as ASSET_NAME (drift would break the gate's checksum lookup).
func TestInstallAssetName(t *testing.T) {
	cases := []struct {
		target, variant, want string
	}{
		{"linux-amd64", "", "promise-linux-amd64.gz"},
		{"linux-amd64", "full", "promise-linux-amd64-full.gz"},
		{"darwin-arm64", "", "promise-darwin-arm64.gz"},
		{"darwin-arm64", "full", "promise-darwin-arm64-full.gz"},
		{"windows-amd64", "", "promise-windows-amd64.exe.gz"},
		{"windows-amd64", "full", "promise-windows-amd64-full.exe.gz"},
	}
	for _, c := range cases {
		if got := installAssetName(c.target, c.variant); got != c.want {
			t.Errorf("installAssetName(%q, %q) = %q, want %q", c.target, c.variant, got, c.want)
		}
	}
}

// TestWriteInstallSums verifies the sha256sum-format output AND the multi-host
// merge: a second call for a different host's assets must preserve the first
// host's lines (each host runs publish-install separately, staging into one
// --out).
func TestWriteInstallSums(t *testing.T) {
	dir := t.TempDir()
	// Host A: darwin assets.
	a1 := filepath.Join(dir, "promise-darwin-arm64.gz")
	a2 := filepath.Join(dir, "promise-darwin-arm64-full.gz")
	mustWrite(t, a1, "AAA")
	mustWrite(t, a2, "BBB")
	if err := writeInstallSums(dir, []string{a1, a2}); err != nil {
		t.Fatal(err)
	}

	// Host B: linux assets, staged into the SAME dir.
	b1 := filepath.Join(dir, "promise-linux-amd64.gz")
	mustWrite(t, b1, "CCC")
	if err := writeInstallSums(dir, []string{b1}); err != nil {
		t.Fatal(err)
	}

	sums := parseSums(t, filepath.Join(dir, "SHA256SUMS"))
	want := map[string]string{
		"promise-darwin-arm64.gz":      sha256Hex([]byte("AAA")),
		"promise-darwin-arm64-full.gz": sha256Hex([]byte("BBB")),
		"promise-linux-amd64.gz":       sha256Hex([]byte("CCC")),
	}
	if !reflect.DeepEqual(sums, want) {
		t.Fatalf("SHA256SUMS = %v, want %v", sums, want)
	}

	// Re-staging host A replaces its lines (new content → new hash) without
	// disturbing host B's.
	mustWrite(t, a1, "AAA2")
	if err := writeInstallSums(dir, []string{a1, a2}); err != nil {
		t.Fatal(err)
	}
	sums = parseSums(t, filepath.Join(dir, "SHA256SUMS"))
	if sums["promise-darwin-arm64.gz"] != sha256Hex([]byte("AAA2")) {
		t.Errorf("re-stage did not update darwin thin hash")
	}
	if sums["promise-linux-amd64.gz"] != sha256Hex([]byte("CCC")) {
		t.Errorf("re-stage clobbered linux hash")
	}

	// Output must be sha256sum format: "<sha>␣␣<name>", sorted by name.
	data, _ := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	var names []string
	for _, l := range lines {
		fields := strings.Fields(l)
		if len(fields) != 2 {
			t.Fatalf("malformed SHA256SUMS line %q", l)
		}
		if !strings.Contains(l, fields[0]+"  "+fields[1]) {
			t.Errorf("line %q is not two-space separated", l)
		}
		names = append(names, fields[1])
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("SHA256SUMS names not sorted: %v", names)
	}
}

// TestCopyInstallScriptLF verifies the publish-install staging guard (T0820):
// a CRLF install.sh must be normalized to LF (no `\r` bytes survive), and an
// already-LF file must be copied unchanged.
func TestCopyInstallScriptLF(t *testing.T) {
	dir := t.TempDir()

	// CRLF input → LF output, no carriage returns.
	crlfSrc := filepath.Join(dir, "crlf.sh")
	mustWrite(t, crlfSrc, "#!/bin/sh\r\nset -eu\r\necho hi\r\n")
	crlfDst := filepath.Join(dir, "out-crlf.sh")
	if err := copyInstallScriptLF(crlfSrc, crlfDst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(crlfDst)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(string(got), '\r') {
		t.Errorf("copyInstallScriptLF left a carriage return in output: %q", got)
	}
	if string(got) != "#!/bin/sh\nset -eu\necho hi\n" {
		t.Errorf("CRLF→LF content = %q, want LF form", got)
	}

	// Already-LF input is unchanged.
	lf := "#!/bin/sh\nset -eu\necho hi\n"
	lfSrc := filepath.Join(dir, "lf.sh")
	mustWrite(t, lfSrc, lf)
	lfDst := filepath.Join(dir, "out-lf.sh")
	if err := copyInstallScriptLF(lfSrc, lfDst); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(lfDst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != lf {
		t.Errorf("LF input changed: got %q, want %q", got, lf)
	}
}

// TestCopyInstallScriptCRLF verifies the publish-install staging guard (T0820):
// install.ps1/install.cmd must be normalized to CRLF regardless of the source's
// line endings, so a Linux host (LF working tree) and a Windows host publish
// byte-identical artifacts. Normalization must be idempotent on CRLF input.
func TestCopyInstallScriptCRLF(t *testing.T) {
	dir := t.TempDir()

	// LF input → CRLF output (every \n becomes \r\n, no lone \n survives).
	lfSrc := filepath.Join(dir, "lf.ps1")
	mustWrite(t, lfSrc, "param()\nWrite-Host hi\n")
	lfDst := filepath.Join(dir, "out-lf.ps1")
	if err := copyInstallScriptCRLF(lfSrc, lfDst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(lfDst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "param()\r\nWrite-Host hi\r\n" {
		t.Errorf("LF→CRLF content = %q, want CRLF form", got)
	}
	if strings.Contains(strings.ReplaceAll(string(got), "\r\n", ""), "\n") {
		t.Errorf("copyInstallScriptCRLF left a lone \\n in output: %q", got)
	}

	// Already-CRLF input is unchanged (idempotent — no doubled \r).
	crlf := "param()\r\nWrite-Host hi\r\n"
	crlfSrc := filepath.Join(dir, "crlf.ps1")
	mustWrite(t, crlfSrc, crlf)
	crlfDst := filepath.Join(dir, "out-crlf.ps1")
	if err := copyInstallScriptCRLF(crlfSrc, crlfDst); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(crlfDst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != crlf {
		t.Errorf("CRLF input changed: got %q, want %q", got, crlf)
	}
}

// TestCopyInstallScript_MissingSource verifies both staging helpers surface the
// read error (rather than silently producing an empty dst) when the source
// script is absent — the staging step in runReleasePublishInstall wraps this
// into a "stage install.sh: ..." failure so a misconfigured tree aborts the
// publish instead of uploading a truncated installer (T0820).
func TestCopyInstallScript_MissingSource(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist.sh")
	dst := filepath.Join(dir, "out.sh")

	if err := copyInstallScriptLF(missing, dst); err == nil {
		t.Error("copyInstallScriptLF with missing source = nil, want error")
	}
	if err := copyInstallScriptCRLF(missing, dst); err == nil {
		t.Error("copyInstallScriptCRLF with missing source = nil, want error")
	}
	// Nothing should have been written for the failed copies.
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("dst should not exist after failed copy, stat err = %v", err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func parseSums(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, l := range strings.Split(string(data), "\n") {
		fields := strings.Fields(l)
		if len(fields) == 2 {
			out[fields[1]] = fields[0]
		}
	}
	return out
}
