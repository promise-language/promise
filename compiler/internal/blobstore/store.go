package blobstore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/promise-language/promise/compiler/internal/module"
)

// Store is the content-addressed cache rooted at <PromiseHome>/cache.
//
//	cache/
//	  blobs/sha256/<hash>      ← materialized dependency blobs (source of truth)
//	  archives/sha256/<hash>   ← persistently cached archives (bandwidth only)
//	  .lock                    ← CAS-wide flock, shared with install/fetch/gc
type Store struct {
	root string // <PromiseHome>/cache
}

// NewStore returns the CAS rooted at <PromiseHome>/cache.
func NewStore() (*Store, error) {
	home, err := module.PromiseHome()
	if err != nil {
		return nil, err
	}
	return &Store{root: filepath.Join(home, "cache")}, nil
}

// NewStoreAt returns a CAS rooted at the given cache directory (tests).
func NewStoreAt(cacheRoot string) *Store { return &Store{root: cacheRoot} }

// Root returns the cache root directory.
func (s *Store) Root() string { return s.root }

func (s *Store) blobsDir() string    { return filepath.Join(s.root, "blobs", "sha256") }
func (s *Store) archivesDir() string { return filepath.Join(s.root, "archives", "sha256") }

// BlobPath returns the on-disk path for a blob hash (whether or not it exists).
func (s *Store) BlobPath(hash string) string {
	return filepath.Join(s.blobsDir(), normalizeHash(hash))
}

// ArchivePath returns the on-disk path for an archive hash.
func (s *Store) ArchivePath(hash string) string {
	return filepath.Join(s.archivesDir(), normalizeHash(hash))
}

// Has reports whether a blob is present in the CAS. Per §4.2 step 2 the cache
// trusts entries by presence (not re-hashed per build); integrity is repaired
// by `promise doctor` (T0771).
func (s *Store) Has(hash string) bool {
	if hash == "" {
		return false
	}
	_, err := os.Stat(s.BlobPath(hash))
	return err == nil
}

// HasArchive reports whether an archive is present in the CAS.
func (s *Store) HasArchive(hash string) bool {
	if hash == "" {
		return false
	}
	_, err := os.Stat(s.ArchivePath(hash))
	return err == nil
}

// commitBlob atomically installs already-verified bytes at tmpPath into
// blobs/sha256/<hash> via rename (§4.4): an interrupted fetch can never leave a
// half-written entry that looks valid by presence. Same temp+rename robustness
// as T0722's install-binary write. Caller must have verified the sha256.
func (s *Store) commitBlob(tmpPath, hash string) (string, error) {
	dst := s.BlobPath(hash)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return "", fmt.Errorf("commit blob %s: %w", hash, err)
	}
	return dst, nil
}

// commitArchive atomically installs a verified archive into archives/sha256/<hash>.
func (s *Store) commitArchive(tmpPath, hash string) (string, error) {
	dst := s.ArchivePath(hash)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return "", fmt.Errorf("commit archive %s: %w", hash, err)
	}
	return dst, nil
}

// StageBlob installs raw bytes (e.g. a full variant's bundled, un-gzipped blob)
// into the CAS at install time. It hashes the bytes, writes them to a temp file,
// and renames into blobs/sha256/<hash>. Returns the content hash. Callers that
// already know the expected hash should compare it against the return value.
func (s *Store) StageBlob(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	if s.Has(hash) {
		return hash, nil
	}
	if err := os.MkdirAll(s.blobsDir(), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(s.blobsDir(), ".stage-*.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	if _, err := s.commitBlob(tmpName, hash); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	return hash, nil
}

// Lock takes the CAS-wide exclusive lock (cache/.lock), shared with
// install/fetch/gc. Mirrors prebuilts.acquireCacheLock and the canonical
// acquireVerifyLockIn (tools/build/common/verify.go). Delegates to the
// path-targeted Lock helper; the OS releases the lock on process death, so stale
// locks are impossible. Returns an unlock func.
func (s *Store) Lock(identityHint string) (func(), error) {
	return Lock(filepath.Join(s.root, ".lock"), identityHint,
		fmt.Sprintf("Waiting for dependency cache lock at %s...", s.root))
}

// Lock acquires an exclusive advisory lock at lockPath (its parent dir is created
// if missing), announcing the current holder before blocking. waitMsg is printed
// when the lock is contended (overridden by the holder's identity hint when the
// sibling <lockPath>.owner file is readable). The OS releases the lock on process
// death, so a crashed holder can never leave a permanently stale lock — no PID/TTL
// bookkeeping is required. Returns an unlock func.
//
// Holder metadata lives in a sibling <lockPath>.owner file, NOT lockPath itself:
// on Windows the file lock is a mandatory byte-range lock on byte 0 of lockPath,
// so a concurrent read/write of lockPath while held fails. The .owner sibling is
// unaffected by the lock and readable on every platform.
func Lock(lockPath, identityHint, waitMsg string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, err
	}
	ownerPath := lockPath + ".owner"
	fl := newFileLock(lockPath)
	locked, err := fl.tryLock()
	if err != nil {
		return nil, fmt.Errorf("acquire lock %s: %w", lockPath, err)
	}
	if !locked {
		msg := waitMsg
		if data, err := os.ReadFile(ownerPath); err == nil {
			if holder := strings.TrimSpace(string(data)); holder != "" {
				msg = fmt.Sprintf("Waiting for %s to finish...", holder)
			}
		}
		fmt.Fprintln(os.Stderr, msg)
		if err := fl.lock(); err != nil {
			return nil, fmt.Errorf("acquire lock %s: %w", lockPath, err)
		}
	}
	_ = os.WriteFile(ownerPath, []byte(identityHint+"\n"), 0o644)
	return func() {
		_ = os.Remove(ownerPath)
		_ = fl.unlock()
	}, nil
}

// EpochRefs computes the blob+archive hash set referenced by a manifest, for
// the per-epoch blobs.refs file (§4.4). Lines are "blob <hash>" / "archive
// <hash>" (archive lines only for sources carrying archive_sha256, the asserted
// content key). The set is derived from the manifest alone — no epoch binary is
// executed. T0771's gc/remove reads the union of all epochs/*/blobs.refs.
func EpochRefs(m *Manifest) []string {
	blobs := map[string]bool{}
	archives := map[string]bool{}
	for _, e := range m.Entries {
		blobs[normalizeHash(e.SHA256)] = true
		for _, src := range e.Sources {
			if h := normalizeHash(src.ArchiveSHA256); h != "" {
				archives[h] = true
			}
		}
	}
	var lines []string
	for h := range blobs {
		lines = append(lines, "blob "+h)
	}
	for h := range archives {
		lines = append(lines, "archive "+h)
	}
	sort.Strings(lines)
	return lines
}

// WriteEpochRefs writes epochs/<epoch>/blobs.refs from the embedded manifest so
// GC can compute roots without executing any epoch binary (§4.4). This is the
// T0769 deliverable T0771 consumes.
func WriteEpochRefs(epochDir string, m *Manifest) error {
	if err := os.MkdirAll(epochDir, 0o755); err != nil {
		return err
	}
	refs := EpochRefs(m)
	content := strings.Join(refs, "\n")
	if content != "" {
		content += "\n"
	}
	return os.WriteFile(filepath.Join(epochDir, "blobs.refs"), []byte(content), 0o644)
}

// ListBlobs returns every well-formed CAS blob keyed by content hash → size.
// Entries whose name is not a 64-char lowercase hex string (in-flight staging
// residue like ".stage-*.tmp" / "blob-*") are skipped — only committed entries
// are listed. A missing blobs dir yields an empty map (not an error).
func (s *Store) ListBlobs() (map[string]int64, error) { return listHashed(s.blobsDir()) }

// ListArchives returns every well-formed cached archive keyed by hash → size.
func (s *Store) ListArchives() (map[string]int64, error) { return listHashed(s.archivesDir()) }

func listHashed(dir string) (map[string]int64, error) {
	out := map[string]int64{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !isHexHash(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out[e.Name()] = info.Size()
	}
	return out, nil
}

// SweepStagingResidue removes in-flight staging temporaries left by a crashed or
// interrupted install/fetch: non-hash entries in blobs/sha256 and archives/sha256
// (".stage-*.tmp") and ".fetch-tmp-*" scratch dirs under the CAS root. Committed
// entries are 64-char hex (isHexHash) and are never touched, so this is safe.
// The caller MUST hold Store.Lock(). Returns the count removed (best-effort;
// individual remove errors other than NotExist are returned, but the sweep
// continues past them). quarantine/, blobs.refs, and *.lock are never touched.
func (s *Store) SweepStagingResidue() (int, error) {
	removed := 0
	var firstErr error
	// Non-hex entries in the CAS blob/archive dirs are staging residue by
	// construction — commitBlob/commitArchive only ever produce hex-named files.
	for _, dir := range []string{s.blobsDir(), s.archivesDir()} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, e := range entries {
			if isHexHash(e.Name()) {
				continue
			}
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil && !os.IsNotExist(err) {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			removed++
		}
	}
	// Resolver scratch dirs live directly under the CAS root (resolve.go).
	rootEntries, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return removed, firstErr
		}
		if firstErr == nil {
			firstErr = err
		}
		return removed, firstErr
	}
	for _, e := range rootEntries {
		if !strings.HasPrefix(e.Name(), ".fetch-tmp-") {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.root, e.Name())); err != nil && !os.IsNotExist(err) {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		removed++
	}
	return removed, firstErr
}

// isHexHash reports whether name is a 64-char lowercase hex sha256 — the exact
// shape of a committed CAS entry's filename. Defends GC/verify against the
// resolver's temp residue (".stage-*", "blob-*", "out-*", "archive-*").
func isHexHash(name string) bool {
	if len(name) != 64 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// ReadEpochRefs parses epochs/<epoch>/blobs.refs into its referenced blob +
// archive hash sets. Returns ok=false (NOT an error) when the file is absent or
// unreadable so callers can apply the §4.4 fail-safe (keep everything rather
// than risk deleting a live blob). Lines are "blob <hash>" / "archive <hash>"
// as written by WriteEpochRefs; malformed lines are ignored.
func ReadEpochRefs(epochDir string) (blobs, archives map[string]bool, ok bool) {
	data, err := os.ReadFile(filepath.Join(epochDir, "blobs.refs"))
	if err != nil {
		return nil, nil, false
	}
	blobs = map[string]bool{}
	archives = map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		h := normalizeHash(fields[1])
		switch fields[0] {
		case "blob":
			blobs[h] = true
		case "archive":
			archives[h] = true
		}
	}
	return blobs, archives, true
}

// hashFile returns the lowercase hex sha256 of a file's contents.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// normalizeHash lowercases and trims a hash for consistent path/comparison use.
func normalizeHash(h string) string { return strings.ToLower(strings.TrimSpace(h)) }
