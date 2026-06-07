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
