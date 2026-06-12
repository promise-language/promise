package blobstore

import (
	"os"
	"path/filepath"
	"testing"
)

// A clean store reports zero corruption.
func TestVerifyCleanStore(t *testing.T) {
	_, s := homeStore(t)
	seedBlob(t, s, "alpha")
	seedBlob(t, s, "beta")
	seedArchive(t, s, "tar")

	res, err := s.Verify(false)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.BlobsChecked != 2 || res.ArchivesChecked != 1 {
		t.Fatalf("checked counts wrong: %+v", res)
	}
	if len(res.Corrupt) != 0 {
		t.Fatalf("clean store reported corruption: %v", res.Corrupt)
	}
}

// A truncated/rewritten blob is detected and, with repair, quarantined so the
// next use re-fetches a clean copy (Has → false). Acceptance bullet 4.
func TestVerifyDetectsAndQuarantinesCorruption(t *testing.T) {
	_, s := homeStore(t)
	good := seedBlob(t, s, "intact")

	// A corrupt entry: filename is the content address of "expected" but the
	// bytes on disk are "tampered".
	badHash := sha256hex([]byte("expected"))
	if err := os.MkdirAll(s.blobsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.BlobPath(badHash), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Verify-only flags it without touching disk.
	res, err := s.Verify(false)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(res.Corrupt) != 1 || res.Corrupt[0].Hash != badHash {
		t.Fatalf("expected 1 corrupt entry %s, got %v", badHash, res.Corrupt)
	}
	if len(res.Quarantined) != 0 {
		t.Fatal("verify-only must not quarantine")
	}
	if !s.Has(badHash) {
		t.Fatal("verify-only must not remove the corrupt entry")
	}

	// Repair quarantines it.
	res, err = s.Verify(true)
	if err != nil {
		t.Fatalf("Verify(repair): %v", err)
	}
	if len(res.Quarantined) != 1 {
		t.Fatalf("expected 1 quarantined, got %v", res.Quarantined)
	}
	if s.Has(badHash) {
		t.Error("corrupt entry must be removed from the live CAS after repair")
	}
	if !s.Has(good) {
		t.Error("repair must not touch intact entries")
	}
	// The bytes are preserved (quarantined, not deleted).
	q := filepath.Join(s.Root(), "quarantine", "blob", badHash+".0")
	if _, err := os.Stat(q); err != nil {
		t.Errorf("quarantined bytes not preserved at %s: %v", q, err)
	}

	// A second repair run on the now-clean store reports nothing.
	res, err = s.Verify(true)
	if err != nil {
		t.Fatalf("Verify(repair) rerun: %v", err)
	}
	if len(res.Corrupt) != 0 {
		t.Errorf("re-scan after repair still reports corruption: %v", res.Corrupt)
	}
}

// A corrupt ARCHIVE entry is detected and quarantined into quarantine/archive/
// (the blob path is symmetric but archives have their own kind/dir).
func TestVerifyDetectsAndQuarantinesCorruptArchive(t *testing.T) {
	_, s := homeStore(t)
	badHash := sha256hex([]byte("expected-archive"))
	if err := os.MkdirAll(s.archivesDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.ArchivePath(badHash), []byte("rotted"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := s.Verify(true)
	if err != nil {
		t.Fatalf("Verify(repair): %v", err)
	}
	if res.ArchivesChecked != 1 {
		t.Fatalf("expected 1 archive checked, got %d", res.ArchivesChecked)
	}
	if len(res.Quarantined) != 1 || res.Quarantined[0].Kind != "archive" {
		t.Fatalf("expected 1 archive quarantined, got %v", res.Quarantined)
	}
	if s.HasArchive(badHash) {
		t.Error("corrupt archive must be removed from the live CAS after repair")
	}
	q := filepath.Join(s.Root(), "quarantine", "archive", badHash+".0")
	if _, err := os.Stat(q); err != nil {
		t.Errorf("quarantined archive bytes not preserved at %s: %v", q, err)
	}
}

// verifyDir scans only committed (hex-named) entries: in-flight staging residue
// and subdirectories are skipped, never counted or flagged corrupt.
func TestVerifyIgnoresResidueAndSubdirs(t *testing.T) {
	_, s := homeStore(t)
	good := seedBlob(t, s, "real")
	// Non-hex staging residue — would hash-mismatch its (non-address) name if scanned.
	if err := os.WriteFile(filepath.Join(s.blobsDir(), ".stage-9.tmp"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A stray subdirectory inside blobs/sha256.
	if err := os.MkdirAll(filepath.Join(s.blobsDir(), "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := s.Verify(false)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.BlobsChecked != 1 {
		t.Fatalf("expected only the committed blob checked, got %d", res.BlobsChecked)
	}
	if len(res.Corrupt) != 0 {
		t.Fatalf("residue/subdir must not be flagged corrupt: %v", res.Corrupt)
	}
	if !s.Has(good) {
		t.Error("the committed blob must remain")
	}
}

// Verifying a store with no blobs/archives dir at all is a clean no-op (the
// IsNotExist branch of verifyDir).
func TestVerifyMissingStore(t *testing.T) {
	_, s := homeStore(t)
	res, err := s.Verify(true)
	if err != nil {
		t.Fatalf("Verify on empty store: %v", err)
	}
	if res.BlobsChecked != 0 || res.ArchivesChecked != 0 || len(res.Corrupt) != 0 {
		t.Fatalf("empty store should report nothing: %+v", res)
	}
}

// A non-IsNotExist ReadDir failure (here: blobs/sha256 is a regular file, not a
// directory → ENOTDIR) propagates as an error rather than being swallowed.
func TestVerifyReadDirError(t *testing.T) {
	_, s := homeStore(t)
	// Create blobs/ but make blobs/sha256 a FILE where a dir is expected.
	if err := os.MkdirAll(filepath.Dir(s.blobsDir()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.blobsDir(), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(false); err == nil {
		t.Fatal("expected a ReadDir error when blobs/sha256 is a file")
	}
}

// Quarantining two distinct corruptions of the SAME content address yields
// distinct .0 / .1 files (the collision-suffix loop) — neither set of forensic
// bytes is clobbered.
func TestQuarantineCollisionSuffix(t *testing.T) {
	_, s := homeStore(t)
	badHash := sha256hex([]byte("expected"))
	if err := os.MkdirAll(s.blobsDir(), 0o755); err != nil {
		t.Fatal(err)
	}

	write := func(body string) {
		if err := os.WriteFile(s.BlobPath(badHash), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("tampered-1")
	if _, err := s.Verify(true); err != nil {
		t.Fatalf("Verify #1: %v", err)
	}
	// Re-corrupt the same address with different bytes and repair again.
	write("tampered-2")
	if _, err := s.Verify(true); err != nil {
		t.Fatalf("Verify #2: %v", err)
	}

	for _, n := range []string{".0", ".1"} {
		p := filepath.Join(s.Root(), "quarantine", "blob", badHash+n)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected quarantined %s to exist: %v", badHash+n, err)
		}
	}
	if s.Has(badHash) {
		t.Error("the second corrupt copy must also leave the live CAS")
	}
}
