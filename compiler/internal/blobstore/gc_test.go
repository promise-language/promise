package blobstore

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// homeStore sets PROMISE_HOME to a fresh temp dir and returns a Store rooted at
// <home>/cache (matching what NewStore() resolves), so module.InstalledEpochs /
// EpochDir and the CAS share one home.
func homeStore(t *testing.T) (home string, s *Store) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("PROMISE_HOME", home)
	store, err := NewStore()
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return home, store
}

func seedBlob(t *testing.T, s *Store, content string) string {
	t.Helper()
	hash := sha256hex([]byte(content))
	if err := os.MkdirAll(s.blobsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.BlobPath(hash), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return hash
}

func seedArchive(t *testing.T, s *Store, content string) string {
	t.Helper()
	hash := sha256hex([]byte(content))
	if err := os.MkdirAll(s.archivesDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.ArchivePath(hash), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return hash
}

func writeEpochRefs(t *testing.T, home, epoch string, lines ...string) {
	t.Helper()
	dir := filepath.Join(home, "epochs", epoch)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "blobs.refs"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Two epochs share one blob; removing one (dropping its dir) must reclaim its
// exclusive blob while the shared blob survives. This is acceptance bullet 3.
func TestSweepSharedBlobSurvives(t *testing.T) {
	home, s := homeStore(t)
	shared := seedBlob(t, s, "shared-llvm")
	exclusiveA := seedBlob(t, s, "only-A")
	exclusiveB := seedBlob(t, s, "only-B")

	writeEpochRefs(t, home, "2026.0", "blob "+shared, "blob "+exclusiveA)
	writeEpochRefs(t, home, "2026.1", "blob "+shared, "blob "+exclusiveB)

	// Simulate `remove 2026.0`: drop its dir, then sweep against the remainder.
	if err := os.RemoveAll(filepath.Join(home, "epochs", "2026.0")); err != nil {
		t.Fatal(err)
	}
	liveB, liveA, ok, err := LiveSet("")
	if err != nil {
		t.Fatalf("LiveSet: %v", err)
	}
	if !ok {
		t.Fatal("expected allRefsReadable=true with one readable epoch")
	}
	res, err := s.Sweep(liveB, liveA, ok, nil, false)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.BlobsRemoved != 1 {
		t.Fatalf("expected 1 blob removed, got %d (%v)", res.BlobsRemoved, res.Removed)
	}
	if !s.Has(shared) {
		t.Error("shared blob was deleted but is still referenced by 2026.1")
	}
	if !s.Has(exclusiveB) {
		t.Error("2026.1's exclusive blob was deleted")
	}
	if s.Has(exclusiveA) {
		t.Error("removed epoch's exclusive blob was not reclaimed")
	}
}

// A missing/unreadable ref set makes GC keep everything (§4.4 fail-safe).
func TestSweepFailSafeOnMissingRefs(t *testing.T) {
	home, s := homeStore(t)
	b1 := seedBlob(t, s, "b1")
	b2 := seedBlob(t, s, "b2")

	writeEpochRefs(t, home, "2026.0", "blob "+b1)
	// 2026.1 exists but has no blobs.refs → unreadable ref set.
	if err := os.MkdirAll(filepath.Join(home, "epochs", "2026.1"), 0o755); err != nil {
		t.Fatal(err)
	}

	liveB, liveA, ok, err := LiveSet("")
	if err != nil {
		t.Fatalf("LiveSet: %v", err)
	}
	if ok {
		t.Fatal("expected allRefsReadable=false when an epoch's refs are missing")
	}
	res, err := s.Sweep(liveB, liveA, ok, nil, false)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.BlobsRemoved != 0 {
		t.Fatalf("fail-safe sweep deleted %d blobs", res.BlobsRemoved)
	}
	if !s.Has(b1) || !s.Has(b2) {
		t.Error("fail-safe must keep every blob")
	}
}

// An archive whose mapped blobs are all materialized is evicted even while an
// epoch still references it (aggressive archive eviction, §4.4).
func TestSweepAggressiveArchiveEviction(t *testing.T) {
	home, s := homeStore(t)
	blob := seedBlob(t, s, "extracted-tool")
	archive := seedArchive(t, s, "the-tarball")

	// The epoch references BOTH — without the aggressive rule the archive would
	// survive as a referenced root.
	writeEpochRefs(t, home, "2026.0", "blob "+blob, "archive "+archive)

	m := mustManifest(t, ManifestEntry{
		Name:   "llvm-opt",
		SHA256: blob,
		Size:   int64(len("extracted-tool")),
		Kind:   KindBlob,
		Sources: []Source{{
			Archive:       "https://example.com/llvm.tar.xz",
			ArchivePath:   "bin/opt",
			ArchiveSHA256: archive,
		}},
	})

	liveB, liveA, ok, err := LiveSet("")
	if err != nil {
		t.Fatalf("LiveSet: %v", err)
	}
	res, err := s.Sweep(liveB, liveA, ok, m, false)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if !s.Has(blob) {
		t.Error("the materialized blob must survive")
	}
	if s.HasArchive(archive) {
		t.Error("a redundant archive (all blobs materialized) should be evicted")
	}
	if res.ArchivesRemoved != 1 {
		t.Errorf("expected 1 archive removed, got %d", res.ArchivesRemoved)
	}
}

// An archive referenced by an epoch whose blob is NOT yet materialized must be
// kept (the aggressive rule only fires when re-fetch is free).
func TestSweepKeepsArchiveWhenBlobMissing(t *testing.T) {
	home, s := homeStore(t)
	archive := seedArchive(t, s, "the-tarball")
	blobHash := sha256hex([]byte("not-yet-extracted")) // intentionally NOT seeded

	writeEpochRefs(t, home, "2026.0", "archive "+archive)

	m := mustManifest(t, ManifestEntry{
		Name:   "llvm-opt",
		SHA256: blobHash,
		Size:   1,
		Kind:   KindBlob,
		Sources: []Source{{
			Archive:       "https://example.com/llvm.tar.xz",
			ArchivePath:   "bin/opt",
			ArchiveSHA256: archive,
		}},
	})

	liveB, liveA, ok, err := LiveSet("")
	if err != nil {
		t.Fatalf("LiveSet: %v", err)
	}
	if _, err := s.Sweep(liveB, liveA, ok, m, false); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if !s.HasArchive(archive) {
		t.Error("archive must be kept while a referenced blob is not materialized")
	}
}

// Dry-run reports without deleting.
func TestSweepDryRun(t *testing.T) {
	home, s := homeStore(t)
	orphan := seedBlob(t, s, "orphan")
	writeEpochRefs(t, home, "2026.0", "blob "+seedBlob(t, s, "kept"))

	liveB, liveA, ok, err := LiveSet("")
	if err != nil {
		t.Fatalf("LiveSet: %v", err)
	}
	res, err := s.Sweep(liveB, liveA, ok, nil, true)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.BlobsRemoved != 1 {
		t.Fatalf("dry-run should report 1 blob, got %d", res.BlobsRemoved)
	}
	if !s.Has(orphan) {
		t.Error("dry-run must not delete anything")
	}
}

// LiveSet with no installed epochs is treated as "cannot establish liveness" →
// keep everything (a fresh dev tree must never have its CAS nuked).
func TestLiveSetNoEpochsKeepsEverything(t *testing.T) {
	_, s := homeStore(t)
	b := seedBlob(t, s, "dev-blob")
	liveB, liveA, ok, err := LiveSet("")
	if err != nil {
		t.Fatalf("LiveSet: %v", err)
	}
	if ok {
		t.Fatal("expected allRefsReadable=false with zero epochs")
	}
	if _, err := s.Sweep(liveB, liveA, ok, nil, false); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if !s.Has(b) {
		t.Error("zero-epoch sweep must keep everything")
	}
}

// LiveSet's excludeEpoch skips a named epoch's refs even while its dir+refs are
// still on disk — the mechanism `remove` relies on to root against the REMAINING
// epochs (the floor path before the dir is unlinked). The excluded epoch's
// exclusive blob then sweeps while the surviving epoch's blob stays.
func TestLiveSetExcludeEpoch(t *testing.T) {
	home, s := homeStore(t)
	shared := seedBlob(t, s, "shared")
	onlyOld := seedBlob(t, s, "only-old")
	onlyNew := seedBlob(t, s, "only-new")

	writeEpochRefs(t, home, "2026.0", "blob "+shared, "blob "+onlyOld)
	writeEpochRefs(t, home, "2026.1", "blob "+shared, "blob "+onlyNew)

	// Both dirs still present, but exclude 2026.0 from the roots.
	liveB, liveA, ok, err := LiveSet("2026.0")
	if err != nil {
		t.Fatalf("LiveSet: %v", err)
	}
	if !ok {
		t.Fatal("expected allRefsReadable=true with the remaining epoch readable")
	}
	if liveB[onlyOld] {
		t.Error("excluded epoch's blob must not be in the live set")
	}
	if !liveB[shared] || !liveB[onlyNew] {
		t.Error("the surviving epoch's blobs must be live")
	}
	res, err := s.Sweep(liveB, liveA, ok, nil, false)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.BlobsRemoved != 1 || !s.Has(shared) || !s.Has(onlyNew) || s.Has(onlyOld) {
		t.Fatalf("excludeEpoch sweep wrong: removed=%d shared=%v new=%v old=%v",
			res.BlobsRemoved, s.Has(shared), s.Has(onlyNew), s.Has(onlyOld))
	}
}

// A manifest source carrying no archive_sha256 contributes no archive→blob
// mapping (gc must not panic or mis-evict on the empty-hash source); the archive
// it doesn't map survives the aggressive rule and is kept by the floor rule
// because the epoch references it.
func TestSweepIgnoresSourceWithoutArchiveHash(t *testing.T) {
	home, s := homeStore(t)
	blob := seedBlob(t, s, "tool")
	archive := seedArchive(t, s, "tarball")

	writeEpochRefs(t, home, "2026.0", "blob "+blob, "archive "+archive)

	// Source has Archive+ArchivePath but NO ArchiveSHA256 → ah == "" path.
	m := mustManifest(t, ManifestEntry{
		Name:   "llvm-opt",
		SHA256: blob,
		Size:   int64(len("tool")),
		Kind:   KindBlob,
		Sources: []Source{{
			Archive:     "https://example.com/llvm.tar.xz",
			ArchivePath: "bin/opt",
			// ArchiveSHA256 intentionally empty
		}},
	})

	liveB, liveA, ok, err := LiveSet("")
	if err != nil {
		t.Fatalf("LiveSet: %v", err)
	}
	res, err := s.Sweep(liveB, liveA, ok, m, false)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if !s.HasArchive(archive) {
		t.Error("a referenced archive with no manifest mapping must be kept (floor rule)")
	}
	if res.ArchivesRemoved != 0 {
		t.Errorf("no archive should be removed, got %d", res.ArchivesRemoved)
	}
}

// An archive in NO installed epoch's ref set is swept by the floor rule even
// with no manifest (the pure-orphan case, complementing the aggressive rule).
func TestSweepOrphanArchive(t *testing.T) {
	home, s := homeStore(t)
	kept := seedBlob(t, s, "kept-blob")
	orphanArchive := seedArchive(t, s, "stale-tarball")

	writeEpochRefs(t, home, "2026.0", "blob "+kept) // references no archive

	liveB, liveA, ok, err := LiveSet("")
	if err != nil {
		t.Fatalf("LiveSet: %v", err)
	}
	res, err := s.Sweep(liveB, liveA, ok, nil, false)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if s.HasArchive(orphanArchive) {
		t.Error("an unreferenced archive must be swept by the floor rule")
	}
	if res.ArchivesRemoved != 1 {
		t.Errorf("expected 1 archive removed, got %d", res.ArchivesRemoved)
	}
	if !s.Has(kept) {
		t.Error("the referenced blob must survive")
	}
}

// Real deletion (non-dry-run) actually unlinks the swept blob from disk.
func TestSweepActuallyDeletesFromDisk(t *testing.T) {
	home, s := homeStore(t)
	orphan := seedBlob(t, s, "orphan")
	writeEpochRefs(t, home, "2026.0", "blob "+seedBlob(t, s, "kept"))

	liveB, liveA, ok, err := LiveSet("")
	if err != nil {
		t.Fatalf("LiveSet: %v", err)
	}
	if _, err := s.Sweep(liveB, liveA, ok, nil, false); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, err := os.Stat(s.BlobPath(orphan)); !os.IsNotExist(err) {
		t.Errorf("swept blob still on disk: %v", err)
	}
}

// SweepStagingResidue reaps crashed-install staging temporaries while leaving
// every committed (hex-named) blob/archive and the .fetch-tmp-* nothing else.
func TestSweepStagingResidue(t *testing.T) {
	home, s := homeStore(t)
	blob := seedBlob(t, s, "real-blob")
	archive := seedArchive(t, s, "real-archive")
	// Staging residue: non-hex temp files in the CAS dirs...
	if err := os.WriteFile(filepath.Join(s.blobsDir(), ".stage-9.tmp"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.archivesDir(), ".stage-7.tmp"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// ...and a resolver scratch dir directly under the CAS root.
	fetchTmp := filepath.Join(s.Root(), ".fetch-tmp-abc")
	if err := os.MkdirAll(filepath.Join(fetchTmp, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	// blobs.refs and .lock must survive untouched.
	if err := os.WriteFile(filepath.Join(s.Root(), ".lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	n, err := s.SweepStagingResidue()
	if err != nil {
		t.Fatalf("SweepStagingResidue: %v", err)
	}
	if n != 3 {
		t.Errorf("expected 3 residue entries removed, got %d", n)
	}
	if _, err := os.Stat(filepath.Join(s.blobsDir(), ".stage-9.tmp")); !os.IsNotExist(err) {
		t.Error("blob staging residue survived the sweep")
	}
	if _, err := os.Stat(filepath.Join(s.archivesDir(), ".stage-7.tmp")); !os.IsNotExist(err) {
		t.Error("archive staging residue survived the sweep")
	}
	if _, err := os.Stat(fetchTmp); !os.IsNotExist(err) {
		t.Error(".fetch-tmp-* scratch dir survived the sweep")
	}
	if _, err := os.Stat(s.BlobPath(blob)); err != nil {
		t.Errorf("committed blob was removed: %v", err)
	}
	if _, err := os.Stat(s.ArchivePath(archive)); err != nil {
		t.Errorf("committed archive was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Root(), ".lock")); err != nil {
		t.Errorf(".lock was removed: %v", err)
	}
	_ = home
}

// A crash before the CAS blob/archive dirs are even created is the common
// self-heal scenario (install error path runs SweepStagingResidue on a barely
// initialized store): missing dirs are not an error, and the sweep is a no-op.
func TestSweepStagingResidueMissingDirs(t *testing.T) {
	_, s := homeStore(t)
	// Fresh store: NewStore does not create blobs/archives dirs until a stage.
	if _, err := os.Stat(s.blobsDir()); !os.IsNotExist(err) {
		t.Skipf("expected blobsDir absent on a fresh store, got %v", err)
	}
	n, err := s.SweepStagingResidue()
	if err != nil {
		t.Fatalf("missing CAS dirs must not be an error: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 removed on an empty store, got %d", n)
	}
}

// A residue entry that cannot be removed (read-only parent dir) is reported via
// the returned error, and the sweep still continues past it (best-effort).
func TestSweepStagingResidueReturnsRemoveError(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Freezing the parent dir with chmod 0o500 does not block child removal on
		// Windows — the read-only directory attribute is not a write-permission
		// gate there, so RemoveAll succeeds and no error is produced. This test
		// exercises the Unix permission model; skip it where that model is absent
		// (T1225).
		t.Skip("read-only parent dir does not block child removal on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}
	_, s := homeStore(t)
	if err := os.MkdirAll(s.blobsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	stuck := filepath.Join(s.blobsDir(), ".stage-stuck.tmp")
	if err := os.WriteFile(stuck, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Also leave a removable .fetch-tmp-* under the root so we prove the sweep
	// keeps going after the un-removable entry.
	fetchTmp := filepath.Join(s.Root(), ".fetch-tmp-keep")
	if err := os.Mkdir(fetchTmp, 0o755); err != nil {
		t.Fatal(err)
	}
	// Freeze the blobs dir so RemoveAll of the child fails with EACCES.
	if err := os.Chmod(s.blobsDir(), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(s.blobsDir(), 0o755) })

	n, err := s.SweepStagingResidue()
	if err == nil {
		t.Fatal("expected a remove error for the read-only residue entry")
	}
	if _, statErr := os.Stat(fetchTmp); !os.IsNotExist(statErr) {
		t.Error("sweep should have continued past the error and removed .fetch-tmp-keep")
	}
	if n != 1 {
		t.Errorf("expected the removable .fetch-tmp-* counted (1), got %d", n)
	}
}

// A live cross-process Resolver holds an exclusive flock on its staging dir's
// .lock sentinel (T1186). SweepStagingResidue must skip that dir — not count it,
// not delete it — so a concurrent doctor --repair cannot pull the dir out from
// under an actively-prefetching resolver. Once the lock is released, the same
// sweep reaps it normally.
func TestSweepStagingResidueSkipsLockedStagingDir(t *testing.T) {
	_, s := homeStore(t)
	blob := seedBlob(t, s, "committed-blob") // must survive both sweeps

	// A live resolver's staging dir: holds an exclusive flock on its sentinel.
	live := filepath.Join(s.Root(), ".fetch-tmp-live")
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatal(err)
	}
	held := newFileLock(filepath.Join(live, stagingLockName))
	if ok, err := held.tryLock(); err != nil || !ok {
		t.Fatalf("tryLock on live staging dir: ok=%v err=%v", ok, err)
	}

	n, err := s.SweepStagingResidue()
	if err != nil {
		t.Fatalf("SweepStagingResidue: %v", err)
	}
	if n != 0 {
		t.Errorf("locked staging dir must not be counted, got %d removed", n)
	}
	if _, err := os.Stat(live); err != nil {
		t.Errorf("locked staging dir was deleted: %v", err)
	}
	if _, err := os.Stat(s.BlobPath(blob)); err != nil {
		t.Errorf("committed blob removed: %v", err)
	}

	// Resolver done: release the flock, then the dir is ordinary residue.
	if err := held.unlock(); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	n, err = s.SweepStagingResidue()
	if err != nil {
		t.Fatalf("SweepStagingResidue (after unlock): %v", err)
	}
	if n != 1 {
		t.Errorf("expected the now-unlocked staging dir removed (1), got %d", n)
	}
	if _, err := os.Stat(live); !os.IsNotExist(err) {
		t.Error("unlocked staging dir survived the sweep")
	}
	if _, err := os.Stat(s.BlobPath(blob)); err != nil {
		t.Errorf("committed blob removed by second sweep: %v", err)
	}
}

// A .fetch-tmp-* dir with no .lock sentinel is the pre-T1186 / crashed-resolver
// shape: no live holder, so it must still be reaped (the OS-released-on-death
// guarantee routes crash recovery through this path). tryLock creates the
// sentinel, acquires it uncontended, and the dir is removed.
func TestSweepStagingResidueRemovesUnlockedStagingDir(t *testing.T) {
	_, s := homeStore(t)
	stale := filepath.Join(s.Root(), ".fetch-tmp-crashed")
	if err := os.MkdirAll(filepath.Join(stale, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	n, err := s.SweepStagingResidue()
	if err != nil {
		t.Fatalf("SweepStagingResidue: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 unlocked staging dir removed, got %d", n)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("unlocked (crashed-resolver) staging dir survived the sweep")
	}
}

// End-to-end T1186: a resolver's live staging dir (created by ensureTmp) carries
// a flocked .lock sentinel that a concurrent SweepStagingResidue leaves intact,
// keeping r.tmpDir valid for the resolver's next Resolve; Close then releases the
// lock and removes the dir, leaking nothing.
func TestResolverStagingDirSurvivesConcurrentSweep(t *testing.T) {
	_, s := homeStore(t)
	if err := os.MkdirAll(s.Root(), 0o755); err != nil {
		t.Fatal(err)
	}
	r := NewResolver(s, &Manifest{})
	defer r.Close()
	if err := r.ensureTmp(); err != nil {
		t.Fatalf("ensureTmp: %v", err)
	}
	if r.tmpDir == "" {
		t.Fatal("ensureTmp did not set tmpDir")
	}
	if _, err := os.Stat(filepath.Join(r.tmpDir, stagingLockName)); err != nil {
		t.Errorf("staging .lock sentinel missing: %v", err)
	}

	// ensureTmp is idempotent: the dir + flock persist across the resolver's many
	// per-blob Resolve calls (the exact lifetime the T1186 race exploited). A
	// second call must reuse the same dir and lock handle, not re-flock.
	dir0, lock0 := r.tmpDir, r.tmpLock
	if err := r.ensureTmp(); err != nil {
		t.Fatalf("ensureTmp (second call): %v", err)
	}
	if r.tmpDir != dir0 || r.tmpLock != lock0 {
		t.Error("second ensureTmp did not reuse the existing staging dir/lock")
	}

	n, err := s.SweepStagingResidue()
	if err != nil {
		t.Fatalf("SweepStagingResidue: %v", err)
	}
	if n != 0 {
		t.Errorf("concurrent sweep must skip the live resolver dir, got %d removed", n)
	}
	if _, err := os.Stat(r.tmpDir); err != nil {
		t.Errorf("live resolver staging dir was deleted by concurrent sweep: %v", err)
	}

	saved := r.tmpDir
	r.Close()
	if _, err := os.Stat(saved); !os.IsNotExist(err) {
		t.Error("Close left the staging dir behind")
	}
	if r.tmpDir != "" || r.tmpLock != nil {
		t.Error("Close did not reset tmpDir/tmpLock")
	}
}

// A .fetch-tmp-* entry that is a regular FILE (not a dir) can never be a live
// resolver's staging dir. Probing its sentinel path opens <file>/.lock, which
// fails with ENOTDIR — the "unusual" tryLock-errors branch (T1186). The sweep
// must fall through conservatively and still reap the entry, exactly as pre-
// T1186 removal did. Guards the lock-probe against regressing into a skip.
func TestSweepStagingResidueReapsPrefixedFile(t *testing.T) {
	_, s := homeStore(t)
	blob := seedBlob(t, s, "committed-blob") // must survive the sweep
	if err := os.MkdirAll(s.Root(), 0o755); err != nil {
		t.Fatal(err)
	}
	// A plain file (not a dir) carrying the staging prefix. Opening its .lock
	// sentinel path => ENOTDIR => tryLock returns a non-nil error.
	stray := filepath.Join(s.Root(), ".fetch-tmp-strayfile")
	if err := os.WriteFile(stray, []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	n, err := s.SweepStagingResidue()
	if err != nil {
		t.Fatalf("SweepStagingResidue: %v", err)
	}
	if n != 1 {
		t.Errorf("expected the prefixed file reaped (1), got %d", n)
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Error("prefixed regular file survived the sweep")
	}
	if _, err := os.Stat(s.BlobPath(blob)); err != nil {
		t.Errorf("committed blob removed: %v", err)
	}
}

// ListBlobs ignores in-flight staging residue (non-hex names).
func TestListBlobsIgnoresTempResidue(t *testing.T) {
	_, s := homeStore(t)
	good := seedBlob(t, s, "real")
	if err := os.WriteFile(filepath.Join(s.blobsDir(), ".stage-123.tmp"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.blobsDir(), "blob-abc"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	blobs, err := s.ListBlobs()
	if err != nil {
		t.Fatalf("ListBlobs: %v", err)
	}
	if len(blobs) != 1 {
		t.Fatalf("expected 1 committed blob, got %d (%v)", len(blobs), blobs)
	}
	if _, ok := blobs[good]; !ok {
		t.Error("the committed blob is missing from the listing")
	}
}
