package blobstore

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CorruptEntry is one CAS entry whose content sha256 no longer matches its
// content address (the filename). Got is the recomputed hash ("" if the file
// could not be read).
type CorruptEntry struct {
	Kind string // "blob" | "archive"
	Hash string // the entry's filename (its asserted content address)
	Path string
	Got  string
}

// VerifyResult reports a CAS integrity scan. Quarantined is populated only when
// Verify was called with repair=true.
type VerifyResult struct {
	BlobsChecked    int
	ArchivesChecked int
	Corrupt         []CorruptEntry
	Quarantined     []CorruptEntry
}

// Verify re-hashes every blobs/ and archives/ entry and compares it against its
// content address (the filename) — the integrity self-healing of §4.4/§6. The
// fetch path trusts the cache by presence (never re-hashed per hit), so this is
// the only thing that turns a bit-rotted / truncated / partially-written CAS
// back into a working one.
//
// When repair is true each failing entry is QUARANTINED (atomically renamed into
// cache/quarantine/<kind>/, the T0722 temp+rename pattern) rather than deleted,
// so Has() returns false for it (the next use re-fetches a clean copy) while the
// broken bytes stay recoverable for forensics. repair MUST be called with
// Store.Lock() held; verify-only needs no lock.
func (s *Store) Verify(repair bool) (VerifyResult, error) {
	var res VerifyResult
	if err := s.verifyDir("blob", s.blobsDir(), repair, &res); err != nil {
		return res, err
	}
	if err := s.verifyDir("archive", s.archivesDir(), repair, &res); err != nil {
		return res, err
	}
	return res, nil
}

func (s *Store) verifyDir(kind, dir string, repair bool, res *VerifyResult) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !isHexHash(name) {
			continue // in-flight staging residue, not a committed CAS entry
		}
		switch kind {
		case "blob":
			res.BlobsChecked++
		default:
			res.ArchivesChecked++
		}
		path := filepath.Join(dir, name)
		got, herr := hashFile(path)
		if herr == nil && strings.EqualFold(got, name) {
			continue // intact
		}
		ce := CorruptEntry{Kind: kind, Hash: name, Path: path, Got: got}
		res.Corrupt = append(res.Corrupt, ce)
		if repair {
			if qerr := s.quarantine(kind, name, path); qerr != nil {
				return qerr
			}
			res.Quarantined = append(res.Quarantined, ce)
		}
	}
	return nil
}

// quarantine atomically renames a corrupt entry into cache/quarantine/<kind>/,
// preserving the broken bytes while removing it from the live CAS. A numeric
// suffix avoids colliding with a prior quarantine of the same content address.
func (s *Store) quarantine(kind, name, path string) error {
	qdir := filepath.Join(s.root, "quarantine", kind)
	if err := os.MkdirAll(qdir, 0o755); err != nil {
		return err
	}
	for n := 0; ; n++ {
		dst := filepath.Join(qdir, fmt.Sprintf("%s.%d", name, n))
		if _, err := os.Lstat(dst); err == nil {
			continue
		}
		return os.Rename(path, dst)
	}
}
