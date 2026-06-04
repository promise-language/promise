package blobstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// OfflineError is the exact §4.4 message emitted when a dependency is uncached
// and the network is unreachable. It points at the remedies owned by T0771
// (the -full build / `promise fetch`).
const OfflineError = "host toolchain not cached and no network; install the -full build or run promise fetch while online"

// Resolver performs one resolution pass over the CAS + manifest. Reuse a single
// Resolver across a batch of Resolve calls (e.g. building the LLVM view dir) so
// a shared archive is downloaded once and bad sources are negative-cached for
// the whole pass (§4.2/§4.3).
type Resolver struct {
	store *Store
	m     *Manifest

	// archiveCache maps an (already-fetched) archive URL → local extracted-root
	// dir for the current pass. Set on first touch; reused by every blob that
	// references the same archive.
	archiveCache map[string]string
	// badSources negative-caches source URLs whose bytes failed verification or
	// download this pass, so wrong/broken bytes aren't re-fetched (§4.3).
	badSources map[string]bool
	// satisfied records entry names already materialized this pass via the
	// materialize-all archive optimization, so the outer loop short-circuits.
	satisfied map[string]bool

	tmpDir string // staging dir on the CAS filesystem (rename is atomic)
}

// NewResolver builds a Resolver for a manifest against a store.
func NewResolver(store *Store, m *Manifest) *Resolver {
	return &Resolver{
		store:        store,
		m:            m,
		archiveCache: map[string]string{},
		badSources:   map[string]bool{},
		satisfied:    map[string]bool{},
	}
}

// Resolve returns the CAS path of the blob for a logical dependency name,
// fetching it if absent. The returned blob is the RAW extracted bytes; macOS
// Mach-O patching happens later when the per-target view dir is built.
func (r *Resolver) Resolve(name string) (string, error) {
	entry, ok := r.m.Lookup(name)
	if !ok {
		return "", fmt.Errorf("dependency %q: not in manifest", name)
	}
	hash := normalizeHash(entry.SHA256)

	// Fast path — CAS hit, lock-free (trusted by presence, §4.2 step 2).
	if r.store.Has(hash) {
		return r.store.BlobPath(hash), nil
	}

	unlock, err := r.store.Lock(fmt.Sprintf("fetch %s", name))
	if err != nil {
		return "", err
	}
	defer unlock()

	// Re-check under the lock — another process may have materialized it.
	if r.store.Has(hash) {
		return r.store.BlobPath(hash), nil
	}

	if err := r.ensureTmp(); err != nil {
		return "", err
	}

	return r.fetch(entry)
}

// ensureTmp lazily creates the per-pass staging dir on the CAS filesystem. It
// lives for the whole Resolver (so a shared archive extracted for one blob is
// reusable by the next), and is removed by Close.
func (r *Resolver) ensureTmp() error {
	if r.tmpDir != "" {
		return nil
	}
	td, err := os.MkdirTemp(r.store.Root(), ".fetch-tmp-")
	if err != nil {
		return err
	}
	r.tmpDir = td
	return nil
}

// Close removes the Resolver's per-pass staging dir. Always defer it after
// NewResolver. Committed CAS blobs/archives live outside tmpDir and survive.
func (r *Resolver) Close() {
	if r.tmpDir != "" {
		os.RemoveAll(r.tmpDir)
		r.tmpDir = ""
	}
}

// fetch walks an entry's ranked sources until one verifies (§4.2 step 3).
func (r *Resolver) fetch(entry *ManifestEntry) (string, error) {
	hash := normalizeHash(entry.SHA256)
	var sawNetworkError bool
	var lastErr error

	for i := range entry.Sources {
		src := entry.Sources[i]
		key := sourceKey(src)
		if r.badSources[key] {
			continue
		}

		var blobTmp string
		var ferr error
		if src.IsArchive() {
			blobTmp, ferr = r.fetchFromArchive(entry, src)
		} else {
			blobTmp, ferr = r.fetchBlob(entry, src)
		}
		if ferr != nil {
			if isNetworkError(ferr) {
				sawNetworkError = true
			}
			lastErr = ferr
			r.badSources[key] = true
			continue
		}

		// Verify the materialized bytes against the content address.
		got, err := hashFile(blobTmp)
		if err != nil {
			lastErr = err
			os.Remove(blobTmp)
			continue
		}
		if !strings.EqualFold(got, hash) {
			info, _ := os.Stat(blobTmp)
			wasted := int64(0)
			if info != nil {
				wasted = info.Size()
			}
			loudMismatch(entry.Name, sourceURL(src), hash, got, wasted)
			reportIntegrityMismatch(entry.Name, sourceURL(src), hash, got, r.m.Epoch)
			r.badSources[key] = true
			os.Remove(blobTmp)
			continue
		}

		path, err := r.store.commitBlob(blobTmp, hash)
		if err != nil {
			os.Remove(blobTmp)
			return "", err
		}
		return path, nil
	}

	// All sources failed. Distinguish "no network" from "broken release".
	if sawNetworkError {
		return "", errors.New(OfflineError)
	}
	return "", fmt.Errorf("dependency %q could not be fetched from any source; the manifest or release may be broken — run promise doctor (last error: %v)", entry.Name, lastErr)
}

// fetchBlob downloads a direct blob source to a temp file, aborting if the
// stream overshoots the manifest size (cheap defense, §4.3).
func (r *Resolver) fetchBlob(entry *ManifestEntry, src Source) (string, error) {
	u := r.rewrite(src.Blob)
	tmp, err := os.CreateTemp(r.tmpDir, "blob-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	tmp.Close()
	if _, err := downloadLimited(u, tmpName, entry.Size); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	return tmpName, nil
}

// fetchFromArchive obtains the shared archive once per pass and extracts the
// requested archive_path. When archive_sha256 is given it is verified before
// paying extraction cost (§4.3) and the archive persistently cached (§4.2).
func (r *Resolver) fetchFromArchive(entry *ManifestEntry, src Source) (string, error) {
	archiveURL := r.rewrite(src.Archive)
	extractRoot, ok := r.archiveCache[archiveURL]
	if !ok {
		root, err := r.obtainArchive(src, archiveURL)
		if err != nil {
			return "", err
		}
		extractRoot = root
		r.archiveCache[archiveURL] = root
		// Materialize-all: opportunistically extract every other manifest entry
		// served by the same archive URL so one download yields opt/llc/lld/etc.
		r.materializeAllFromArchive(archiveURL, extractRoot)
	}

	member, err := resolveMember(extractRoot, src.ArchivePath)
	if err != nil {
		return "", err
	}
	out, err := r.copyOut(member)
	if err != nil {
		return "", fmt.Errorf("extract %s from archive: %w", src.ArchivePath, err)
	}
	return out, nil
}

// resolveMember locates an archive member by its inner-relative path, tolerating
// the single top-level wrapper dir that upstream tarballs use (e.g.
// LLVM-22.1.0-macOS-ARM64/bin/opt for archive_path "bin/opt"). Mirrors
// tools/build/common.resolveInnerRoot.
func resolveMember(extractRoot, archivePath string) (string, error) {
	clean := filepath.Clean(archivePath)
	if direct := filepath.Join(extractRoot, clean); existsPath(direct) {
		return direct, nil
	}
	entries, err := os.ReadDir(extractRoot)
	if err == nil {
		var dirs []string
		hasFile := false
		for _, e := range entries {
			if e.IsDir() {
				dirs = append(dirs, e.Name())
			} else {
				hasFile = true
			}
		}
		if len(dirs) == 1 && !hasFile {
			if cand := filepath.Join(extractRoot, dirs[0], clean); existsPath(cand) {
				return cand, nil
			}
		}
	}
	return "", fmt.Errorf("archive member %q not found under %s", archivePath, extractRoot)
}

func existsPath(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// obtainArchive returns the extracted-root dir for an archive, fetching and
// extracting it (verifying archive_sha256 if present) and persistently caching
// it when an asserted content key exists.
func (r *Resolver) obtainArchive(src Source, archiveURL string) (string, error) {
	archiveHash := normalizeHash(src.ArchiveSHA256)

	// Persistent archive cache hit (only meaningful with an asserted key).
	if archiveHash != "" && r.store.HasArchive(archiveHash) {
		return r.extractArchiveFile(r.store.ArchivePath(archiveHash), archiveURL)
	}

	tmp, err := os.CreateTemp(r.tmpDir, "archive-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	tmp.Close()
	got, err := downloadLimited(archiveURL, tmpName, 0)
	if err != nil {
		os.Remove(tmpName)
		return "", err
	}
	if archiveHash != "" && !strings.EqualFold(got, archiveHash) {
		info, _ := os.Stat(tmpName)
		wasted := int64(0)
		if info != nil {
			wasted = info.Size()
		}
		loudMismatch("archive "+filepath.Base(archiveURL), archiveURL, archiveHash, got, wasted)
		os.Remove(tmpName)
		return "", fmt.Errorf("archive sha256 mismatch for %s", archiveURL)
	}

	archivePath := tmpName
	if archiveHash != "" {
		// Asserted key → persist for cross-run reuse.
		if committed, err := r.store.commitArchive(tmpName, archiveHash); err == nil {
			archivePath = committed
		}
	}
	return r.extractArchiveFile(archivePath, archiveURL)
}

// extractArchiveFile extracts an archive into a fresh dir under the pass tmp.
func (r *Resolver) extractArchiveFile(archivePath, archiveURL string) (string, error) {
	dest, err := os.MkdirTemp(r.tmpDir, "extract-")
	if err != nil {
		return "", err
	}
	if err := extractArchive(archivePath, dest); err != nil {
		return "", fmt.Errorf("extract archive %s: %w", archiveURL, err)
	}
	return dest, nil
}

// materializeAllFromArchive verifies+commits every not-yet-present manifest
// entry whose first source references the same archive URL, from the single
// already-extracted tree (§4.2 materialize-all optimization). Best-effort:
// failures here are ignored — the entry is fetched normally when later resolved.
func (r *Resolver) materializeAllFromArchive(archiveURL, extractRoot string) {
	for i := range r.m.Entries {
		e := &r.m.Entries[i]
		hash := normalizeHash(e.SHA256)
		if r.store.Has(hash) || r.satisfied[e.Name] {
			continue
		}
		for _, s := range e.Sources {
			if !s.IsArchive() || r.rewrite(s.Archive) != archiveURL {
				continue
			}
			member, err := resolveMember(extractRoot, s.ArchivePath)
			if err != nil {
				break
			}
			out, err := r.copyOut(member)
			if err != nil {
				break
			}
			got, err := hashFile(out)
			if err != nil || !strings.EqualFold(got, hash) {
				os.Remove(out)
				break
			}
			if _, err := r.store.commitBlob(out, hash); err != nil {
				os.Remove(out)
				break
			}
			r.satisfied[e.Name] = true
			break
		}
	}
}

// copyOut copies an extracted file (resolving symlinks) into a temp file on the
// CAS filesystem so it can be atomically renamed in.
func (r *Resolver) copyOut(srcPath string) (string, error) {
	real, err := filepath.EvalSymlinks(srcPath)
	if err != nil {
		return "", err
	}
	in, err := os.Open(real)
	if err != nil {
		return "", err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(r.tmpDir, "out-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, in); err != nil {
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
	return tmpName, nil
}

// rewrite applies the PROMISE_BLOB_MIRROR base-URL override (§4.1): the scheme +
// host of every source URL is replaced with the mirror base, preserving the
// path. Enables corporate mirrors / air-gapped installs without a rebuild.
func (r *Resolver) rewrite(rawURL string) string { return rewriteSource(rawURL) }

func rewriteSource(rawURL string) string {
	base := strings.TrimSpace(os.Getenv("PROMISE_BLOB_MIRROR"))
	if base == "" || rawURL == "" {
		return rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	b, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		return rawURL
	}
	u.Scheme = b.Scheme
	u.Host = b.Host
	u.Path = strings.TrimRight(b.Path, "/") + u.Path
	return u.String()
}

func sourceURL(s Source) string {
	if s.IsArchive() {
		return rewriteSource(s.Archive)
	}
	return rewriteSource(s.Blob)
}

func sourceKey(s Source) string {
	if s.IsArchive() {
		return "archive:" + s.Archive + "#" + s.ArchivePath
	}
	return "blob:" + s.Blob
}

// loudMismatch warns with full detail on a content mismatch (§4.3).
func loudMismatch(name, srcURL, expected, got string, bytesWasted int64) {
	fmt.Fprintf(os.Stderr,
		"warning: content mismatch for %q\n  source:   %s\n  expected: %s\n  actual:   %s\n  wasted:   %d bytes (this source rejected; trying next)\n",
		name, srcURL, expected, got, bytesWasted)
}

// downloadLimited streams url → dst hashing in one pass, aborting if the stream
// overshoots sizeLimit (when > 0). Returns the lowercase hex sha256.
func downloadLimited(rawURL, dst string, sizeLimit int64) (string, error) {
	resp, err := http.Get(rawURL)
	if err != nil {
		return "", &netError{err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d (%s) for %s", resp.StatusCode, resp.Status, rawURL)
	}
	f, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	var reader io.Reader = resp.Body
	if sizeLimit > 0 {
		// Read one extra byte so an overshoot is detected rather than silently
		// truncated.
		reader = io.LimitReader(resp.Body, sizeLimit+1)
	}
	n, err := io.Copy(io.MultiWriter(h, f), reader)
	if err != nil {
		return "", err
	}
	if sizeLimit > 0 && n > sizeLimit {
		return "", fmt.Errorf("download overshoots manifest size %d for %s", sizeLimit, rawURL)
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// netError marks a transport-level failure (DNS/connection), used to choose the
// offline error message over the broken-release message.
type netError struct{ err error }

func (e *netError) Error() string { return e.err.Error() }
func (e *netError) Unwrap() error { return e.err }

func isNetworkError(err error) bool {
	var ne *netError
	return errors.As(err, &ne)
}

// Resolve is a convenience for one-off resolution (no batch sharing).
func Resolve(store *Store, m *Manifest, name string) (string, error) {
	r := NewResolver(store, m)
	defer r.Close()
	return r.Resolve(name)
}
