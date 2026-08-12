package common

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// blob_fetch.go acquires one content-addressed dependency blob (`<sha>.br`) from
// the best source available on this host (#7).
//
// It used to be `gh release download` and nothing else: `ensureSlimBlobs` always
// passes a non-empty deps tag, so the plain-HTTP branch at the bottom of
// ghCLIFetcher.FetchAsset was unreachable for every real blob. That made an
// authenticated `gh` a hard requirement of `bin/build` — a fresh clone in a
// clean container could not build at all, which is the opposite of the
// self-fetching toolchain T0530 was working toward.
//
// Two things make credentials unnecessary now: the repo is public (release
// assets are anonymously downloadable), and `publish-blobs` mirrors every blob
// to the public flat CAS bucket at blobMirrorBase. So the sources are ranked and
// each is optional:
//
//	0. $PROMISE_BLOB_MIRROR/<sha>.br   — when set; explicit intent wins outright
//	1. gh release download             — when `gh` is installed AND authenticated
//	2. <releaseAssetBase>/<tag>/<sha>.br — anonymous HTTPS, public repo
//	3. <blobMirrorBase>/<sha>.br       — anonymous HTTPS, public R2 mirror
//
// Trying an unauthenticated source is safe by construction: every caller ends in
// decompressAndVerify against the catalog's uncompressed sha256, so a wrong,
// truncated, or tampered blob fails exactly as a corrupt `gh` download would.
// The sha is the trust anchor — not the transport.

// blobHTTPClient is the shared client for anonymous blob downloads.
//
// No overall Client.Timeout: an LLVM blob is ~40 MB and a slow-but-healthy link
// must not be killed mid-body. The bounds are on the phases that can hang
// without transferring anything (dial, TLS, waiting for response headers), which
// catches a black-holed connection while leaving a legitimate long download
// alone. Proxy comes from the environment so corporate/pass-through proxies work
// without extra configuration.
var blobHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	},
}

// ghAuthOnce memoizes the auth probe — FetchAsset runs once per blob (4+ times
// for one LLVM fetch) and `gh auth status` is a process spawn.
var ghAuthOnce sync.Once
var ghAuthCached bool

// ghAuthAvailable reports whether `gh` is both installed and authenticated. A
// var so tests can stub it.
//
// This is checked BEFORE attempting the gh source rather than after it fails,
// because a missing credential is not transient: unauthenticated, `gh` fails
// instantly with "could not find any host configurations", and running that
// through retryTransient burns the full backoff schedule per blob (3s + 6s + 9s,
// times every file) before reaching a source that would have worked.
var ghAuthAvailable = func() bool {
	ghAuthOnce.Do(func() {
		if _, err := exec.LookPath("gh"); err != nil {
			return
		}
		cmd := exec.Command("gh", "auth", "status")
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		ghAuthCached = cmd.Run() == nil
	})
	return ghAuthCached
}

// ghDownloadAsset downloads one release asset into dir via the gh CLI. A var so
// tests can stub the subprocess.
var ghDownloadAsset = func(tag, asset, dir string) error {
	return retryTransient(fmt.Sprintf("gh release download %s %s", tag, asset),
		blobFetchAttempts, blobFetchBackoff, func() error {
			// A fresh *exec.Cmd per attempt (exec.Cmd is single-use).
			cmd := exec.Command("gh", "release", "download", tag, "-p", asset, "-D", dir, "--clobber")
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			return cmd.Run()
		})
}

// anonymousBlobURLs returns the credential-free URLs for a blob, in the order
// they should be tried.
//
// `asset` is the blob's basename (`<sha>.br`), which IS its content identity —
// hence the mirror needs no path, while the GitHub primary carries the
// /releases/download/<tag>/ prefix that GitHub's asset layout forces on us.
//
// A PROMISE_BLOB_MIRROR override is placed FIRST and replaces the default
// mirror. Setting it is an explicit statement about where blobs should come from
// (corporate mirror, air-gapped cache), and on an air-gapped host the GitHub
// attempt could only ever be dead latency ahead of the source that works.
// Matches rewriteBlobSource in compiler/internal/blobstore/resolve.go: a blob
// mirror is a FLAT sha-keyed namespace, so only the basename is appended.
func anonymousBlobURLs(tag, asset string) []string {
	primary := releaseAssetBase + "/" + tag + "/" + asset
	if override := strings.TrimSpace(os.Getenv("PROMISE_BLOB_MIRROR")); override != "" {
		return []string{strings.TrimRight(override, "/") + "/" + asset, primary}
	}
	return []string{primary, blobMirrorBase + "/" + asset}
}

// fetchBlobRanked walks the ranked sources until one yields the blob, and
// reports every attempt when they all fail — a single "exit status 1" tells a
// user nothing about which of four paths broke or why.
func fetchBlobRanked(tag, asset, dst string) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	type source struct {
		label string
		fetch func() error
	}
	var sources []source

	urls := anonymousBlobURLs(tag, asset)
	ghSource := source{
		label: "gh release download " + tag,
		fetch: func() error {
			if err := ghDownloadAsset(tag, asset, dir); err != nil {
				return err
			}
			// gh writes <dir>/<asset>; move it into place if dst differs.
			if downloaded := filepath.Join(dir, asset); downloaded != dst {
				return os.Rename(downloaded, dst)
			}
			return nil
		},
	}

	// An explicit PROMISE_BLOB_MIRROR outranks gh; otherwise gh (when usable)
	// leads, since an authenticated CLI also covers private forks.
	if os.Getenv("PROMISE_BLOB_MIRROR") != "" {
		// Bind the URL to a local: the closure must not read urls[0] after the
		// reslice below moves it.
		override := urls[0]
		sources = append(sources, source{override, func() error { return fetchHTTPSource(override, dst) }})
		urls = urls[1:]
	}
	if ghAuthAvailable() {
		sources = append(sources, ghSource)
	}
	for _, u := range urls {
		sources = append(sources, source{u, func() error { return fetchHTTPSource(u, dst) }})
	}

	var notes []string
	if !ghAuthAvailable() {
		notes = append(notes, "gh release download: skipped (gh not installed or not authenticated)")
	}
	for i, s := range sources {
		err := s.fetch()
		if err == nil {
			return nil
		}
		notes = append(notes, s.label+": "+err.Error())
		if i < len(sources)-1 {
			fmt.Fprintf(os.Stderr, "blob source failed (%s): %v; trying next source\n", s.label, err)
		}
	}
	return fmt.Errorf("could not fetch %s from any source:\n  %s", asset, strings.Join(notes, "\n  "))
}

// permanentFetchError marks an HTTP status that will not change on retry (404 on
// an unpublished blob, 403 on a private repo). Retrying it only delays the next
// ranked source.
type permanentFetchError struct{ msg string }

func (e permanentFetchError) Error() string { return e.msg }

// fetchHTTPSource downloads rawURL to dst with the same retry budget as the gh
// path, but bails out immediately on a permanent status so a 404 costs one
// request rather than the whole backoff schedule.
func fetchHTTPSource(rawURL, dst string) error {
	var last error
	for attempt := 1; attempt <= blobFetchAttempts; attempt++ {
		err := httpDownloadBlob(rawURL, dst)
		if err == nil {
			return nil
		}
		last = err
		var perm permanentFetchError
		if errors.As(err, &perm) {
			return err
		}
		if attempt < blobFetchAttempts {
			wait := blobFetchBackoff * time.Duration(attempt)
			fmt.Fprintf(os.Stderr, "GET %s failed (attempt %d/%d): %v; retrying in %s\n",
				rawURL, attempt, blobFetchAttempts, err, wait)
			sleepFn(wait)
		}
	}
	return last
}

// httpDownloadBlob GETs rawURL into dst. Writes to a temp file and renames, so a
// failed or partial download never leaves a half-written blob at dst for the
// next ranked source (or a later cache probe) to trip over.
func httpDownloadBlob(rawURL, dst string) error {
	resp, err := blobHTTPClient.Get(rawURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("HTTP %d for %s", resp.StatusCode, rawURL)
		// 5xx and 429 are worth another try; a definitive 4xx is not.
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			return permanentFetchError{msg}
		}
		return fmt.Errorf("%s", msg)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".part-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
