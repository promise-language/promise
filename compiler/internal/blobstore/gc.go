package blobstore

import (
	"os"

	"github.com/promise-language/promise/compiler/internal/module"
)

// LiveSet computes the union of every installed epoch's blobs.refs — the GC root
// set per docs/distribution.md §4.4. excludeEpoch, when non-empty, is skipped
// (used by `remove` to root against the REMAINING epochs after a directory is
// dropped). The roots are derived from epochs/*/blobs.refs alone — no epoch
// binary is executed.
//
// allRefsReadable is the §4.4 fail-safe signal: it is false when ANY installed
// epoch's ref set is missing/unreadable, OR when no epoch contributed a readable
// ref set at all (so liveness can't be established). Callers MUST keep
// everything in that case — over-retention is recoverable; over-deletion wedges
// an epoch's offline build. The partial union is still returned for reporting.
func LiveSet(excludeEpoch string) (liveBlobs, liveArchives map[string]bool, allRefsReadable bool, err error) {
	liveBlobs = map[string]bool{}
	liveArchives = map[string]bool{}
	epochs, err := module.InstalledEpochs()
	if err != nil {
		return liveBlobs, liveArchives, false, err
	}
	allRefsReadable = true
	counted := 0
	for _, ep := range epochs {
		if ep == excludeEpoch {
			continue
		}
		dir, derr := module.EpochDir(ep)
		if derr != nil {
			allRefsReadable = false
			continue
		}
		blobs, archives, ok := ReadEpochRefs(dir)
		if !ok {
			allRefsReadable = false
			continue
		}
		counted++
		for h := range blobs {
			liveBlobs[h] = true
		}
		for h := range archives {
			liveArchives[h] = true
		}
	}
	if counted == 0 {
		// No readable ref set anywhere → cannot establish a live root. Keep all.
		allRefsReadable = false
	}
	return liveBlobs, liveArchives, allRefsReadable, nil
}

// SweepResult reports what a sweep removed (or, in dry-run, would remove).
type SweepResult struct {
	BlobsRemoved    int
	ArchivesRemoved int
	BytesFreed      int64
	Removed         []string // "blob <hash>" / "archive <hash>", for reporting
}

// Sweep deletes CAS blobs/archives referenced by no installed epoch's ref set.
// The caller MUST hold Store.Lock() (so a half-installed epoch whose blobs.refs
// isn't on disk yet can't be swept out from under it). When allRefsReadable is
// false this is a no-op (§4.4 fail-safe). dryRun reports without deleting.
//
// Archives are evicted more aggressively (§4.4): a cached archive is pure
// bandwidth optimization (the per-blob sha256 re-fetches), so it is also swept
// when every blob the manifest maps to it is already materialized — even while
// an epoch still references it. This has no over-deletion risk: an archive is
// never a source of truth, so the worst case is a re-download. manifest may be
// nil (skips the aggressive rule; the floor "in no epoch's archive ref set"
// rule still applies).
func (s *Store) Sweep(liveBlobs, liveArchives map[string]bool, allRefsReadable bool, m *Manifest, dryRun bool) (SweepResult, error) {
	var res SweepResult
	if !allRefsReadable {
		return res, nil
	}

	blobs, err := s.ListBlobs()
	if err != nil {
		return res, err
	}
	for hash, size := range blobs {
		if liveBlobs[hash] {
			continue
		}
		res.Removed = append(res.Removed, "blob "+hash)
		res.BlobsRemoved++
		res.BytesFreed += size
		if !dryRun {
			if err := os.Remove(s.BlobPath(hash)); err != nil && !os.IsNotExist(err) {
				return res, err
			}
		}
	}

	// archive_sha256 → set of blob hashes that archive can materialize.
	archiveBlobs := map[string]map[string]bool{}
	if m != nil {
		for _, e := range m.Entries {
			bh := normalizeHash(e.SHA256)
			for _, src := range e.Sources {
				ah := normalizeHash(src.ArchiveSHA256)
				if ah == "" {
					continue
				}
				if archiveBlobs[ah] == nil {
					archiveBlobs[ah] = map[string]bool{}
				}
				archiveBlobs[ah][bh] = true
			}
		}
	}

	archives, err := s.ListArchives()
	if err != nil {
		return res, err
	}
	for hash, size := range archives {
		sweep := !liveArchives[hash] // floor rule: in no epoch's archive ref set
		if !sweep {
			// Aggressive rule: referenced, but every blob it provides is already
			// materialized → the archive is redundant.
			if mapped := archiveBlobs[hash]; len(mapped) > 0 {
				allPresent := true
				for bh := range mapped {
					if !s.Has(bh) {
						allPresent = false
						break
					}
				}
				sweep = allPresent
			}
		}
		if !sweep {
			continue
		}
		res.Removed = append(res.Removed, "archive "+hash)
		res.ArchivesRemoved++
		res.BytesFreed += size
		if !dryRun {
			if err := os.Remove(s.ArchivePath(hash)); err != nil && !os.IsNotExist(err) {
				return res, err
			}
		}
	}
	return res, nil
}
