package module

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// marker mirrors what the Go linker writes ahead of the build ID. Its presence
// in the hashed window is the precondition that makes hashing a prefix sound.
const testMarker = "\xff Go build ID: \"abc/def\"\n \xff"

func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestCompilerIdentityShape locks the contract every cache key depends on: 128
// bits, as 32 hex characters, and stable within a process.
func TestCompilerIdentityShape(t *testing.T) {
	id := CompilerIdentity()
	if len(id) != 32 {
		t.Errorf("identity = %q (%d chars), want 32 hex chars (128 bits)", id, len(id))
	}
	if strings.Trim(id, "0123456789abcdef") != "" {
		t.Errorf("identity = %q, want lowercase hex only", id)
	}
	if again := CompilerIdentity(); again != id {
		t.Errorf("identity not stable within a process: %q then %q", id, again)
	}
}

// TestCompilerIdentityHashesPrefixWhenMarkerPresent — with the marker in the
// window, the identity is the prefix hash. That is the fast path, and it is
// sound because the window carries the build ID, itself derived from the whole
// binary.
func TestCompilerIdentityHashesPrefixWhenMarkerPresent(t *testing.T) {
	head := make([]byte, compilerIdentityBytes)
	copy(head, "MZ-or-whatever-header")
	copy(head[4096:], testMarker)

	body := append(append([]byte{}, head...), []byte("trailing content far past the window")...)
	p := writeFile(t, "withmarker", body)

	sum := sha256.Sum256(head)
	want := hex.EncodeToString(sum[:])[:32]
	got, err := compilerIdentityOf(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("identity = %s, want the 32KB prefix hash %s", got, want)
	}
}

// TestCompilerIdentityRejectsBinaryWithoutBuildID — the prefix identifies the
// whole binary only because the build ID sits in it. Without the marker that no
// longer holds, and the hash would describe headers alone, where two different
// compilers could collide — a stale cache HIT, i.e. wrong output.
//
// So this is an error, deliberately NOT a fallback to hashing the whole file. A
// fallback would be a second path taken only when the first had quietly stopped
// being valid, which is the failure nobody notices. If this fires, the
// assumption has broken and the scheme should be changed on purpose.
func TestCompilerIdentityRejectsBinaryWithoutBuildID(t *testing.T) {
	head := make([]byte, compilerIdentityBytes) // no marker anywhere
	p := writeFile(t, "nomarker", append(head, []byte("payload")...))

	if _, err := compilerIdentityOf(p); err == nil {
		t.Error("expected an error for a binary with no Go build ID in the window")
	} else if !strings.Contains(err.Error(), "no Go build ID") {
		t.Errorf("error should name the missing build ID, got: %v", err)
	}
}

// TestCompilerIdentityDocumentsPrefixLimit pins what the identity does NOT
// promise. With the marker present, two files identical in their first 32KB are
// indistinguishable — the identity discriminates BUILDS (a rebuild changes the
// build ID and LC_UUID inside the window) but is not tamper detection: a binary
// edited after linking keeps its build ID.
//
// That is the same trade the Go toolchain makes by storing contentID to avoid
// hashing whole binaries. If this ever needs to detect post-link edits, this
// test is the one that has to change, deliberately.
func TestCompilerIdentityDocumentsPrefixLimit(t *testing.T) {
	head := make([]byte, compilerIdentityBytes)
	copy(head[4096:], testMarker)

	a := append(append([]byte{}, head...), []byte("original tail")...)
	b := append(append([]byte{}, head...), []byte("modified tail")...)
	pa, pb := writeFile(t, "a", a), writeFile(t, "b", b)

	ida, _ := compilerIdentityOf(pa)
	idb, _ := compilerIdentityOf(pb)
	if ida != idb {
		t.Errorf("identities differ — the prefix-hash limit this test documents " +
			"no longer holds; if that was deliberate, update this test")
	}
}

// TestCompilerIdentityUnreadableFile — an identity we cannot compute must be an
// error, never a plausible-looking value that would key a cache incorrectly.
// CompilerIdentity turns this into a fatal, since every compiler-keyed cache
// depends on the value.
func TestCompilerIdentityUnreadableFile(t *testing.T) {
	if _, err := compilerIdentityOf(filepath.Join(t.TempDir(), "nonexistent")); err == nil {
		t.Error("expected an error for a file that cannot be read")
	}
}
