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
// install/fetch/gc. Mirrors prebuilts.acquireCacheLock: TryLock, then announce
// + block. The OS releases the lock on process death, so stale locks are
// impossible. Returns an unlock func.
func (s *Store) Lock(identityHint string) (func(), error) {
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(s.root, ".lock")
	fl := newFileLock(lockPath)
	locked, err := fl.tryLock()
	if err != nil {
		return nil, fmt.Errorf("acquire lock %s: %w", lockPath, err)
	}
	if !locked {
		msg := fmt.Sprintf("Waiting for dependency cache lock at %s...", s.root)
		if data, err := os.ReadFile(lockPath); err == nil {
			if holder := strings.TrimSpace(string(data)); holder != "" {
				msg = fmt.Sprintf("Waiting for %s to finish...", holder)
			}
		}
		fmt.Fprintln(os.Stderr, msg)
		if err := fl.lock(); err != nil {
			return nil, fmt.Errorf("acquire lock %s: %w", lockPath, err)
		}
	}
	_ = os.WriteFile(lockPath, []byte(identityHint+"\n"), 0o644)
	return func() {
		_ = os.WriteFile(lockPath, nil, 0o644)
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
