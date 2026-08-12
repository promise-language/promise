package common

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// blob_fetch_test.go pins the ranked, credential-optional blob acquisition (#7).
// The property that matters most: `bin/build` must fetch blobs on a host with no
// GitHub credentials at all, because requiring them made a fresh clone in a
// clean container unbuildable.

// stubGhAuth forces the gh-availability probe for the duration of a test. The
// real probe shells out to `gh auth status`, so a test that did not stub it
// would pass or fail depending on the developer's login state.
func stubGhAuth(t *testing.T, available bool) {
	t.Helper()
	prev := ghAuthAvailable
	ghAuthAvailable = func() bool { return available }
	t.Cleanup(func() { ghAuthAvailable = prev })
}

// stubGhDownload swaps the gh subprocess for fn.
func stubGhDownload(t *testing.T, fn func(tag, asset, dir string) error) {
	t.Helper()
	prev := ghDownloadAsset
	ghDownloadAsset = fn
	t.Cleanup(func() { ghDownloadAsset = prev })
}

// noSleep removes the retry backoff so a retry test runs instantly.
func noSleep(t *testing.T) {
	t.Helper()
	prev := sleepFn
	sleepFn = func(time.Duration) {}
	t.Cleanup(func() { sleepFn = prev })
}

// failGhDownload is the stub for "gh must never be invoked" assertions.
func failGhDownload(t *testing.T) {
	t.Helper()
	stubGhDownload(t, func(tag, asset, dir string) error {
		t.Errorf("gh release download called unexpectedly (tag=%q asset=%q)", tag, asset)
		return fmt.Errorf("unexpected gh invocation")
	})
}

func TestAnonymousBlobURLs_Default(t *testing.T) {
	t.Setenv("PROMISE_BLOB_MIRROR", "")
	got := anonymousBlobURLs("deps-llvm-22.1.0", "abc.br")
	want := []string{
		releaseAssetBase + "/deps-llvm-22.1.0/abc.br",
		blobMirrorBase + "/abc.br",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d urls %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("url[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestAnonymousBlobURLs_MirrorOverride pins that PROMISE_BLOB_MIRROR leads and
// REPLACES the default mirror (it is an explicit statement of where blobs come
// from), and that it is treated as a flat CAS namespace — basename only, no
// /releases/download/<tag>/ path — matching rewriteBlobSource in the runtime.
func TestAnonymousBlobURLs_MirrorOverride(t *testing.T) {
	t.Setenv("PROMISE_BLOB_MIRROR", "https://mirror.corp/promise/")
	got := anonymousBlobURLs("deps-llvm-22.1.0", "abc.br")
	if len(got) != 2 {
		t.Fatalf("got %v, want 2 urls", got)
	}
	if got[0] != "https://mirror.corp/promise/abc.br" {
		t.Errorf("override url = %q, want flat CAS path with trailing slash trimmed", got[0])
	}
	if got[1] != releaseAssetBase+"/deps-llvm-22.1.0/abc.br" {
		t.Errorf("second url = %q, want the GitHub primary", got[1])
	}
	for _, u := range got {
		if strings.HasPrefix(u, blobMirrorBase) {
			t.Errorf("default mirror %q should be replaced by the override, got %v", blobMirrorBase, got)
		}
	}
}

// TestFetchBlobRanked_NoGhCredentials is the regression test for #7: with no
// usable gh, the blob must still arrive over anonymous HTTP.
func TestFetchBlobRanked_NoGhCredentials(t *testing.T) {
	noSleep(t)
	stubGhAuth(t, false)
	failGhDownload(t)

	const body = "brotli-bytes"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/abc.br" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	t.Setenv("PROMISE_BLOB_MIRROR", srv.URL)

	dst := filepath.Join(t.TempDir(), "abc.br")
	if err := fetchBlobRanked("deps-llvm-22.1.0", "abc.br", dst); err != nil {
		t.Fatalf("fetchBlobRanked: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("content = %q, want %q", got, body)
	}
}

// TestFetchBlobRanked_PrefersGhWhenAuthenticated keeps the existing flow intact:
// an authenticated CLI still leads (it is the only path that works for a private
// fork), so the HTTP sources must not be touched.
func TestFetchBlobRanked_PrefersGhWhenAuthenticated(t *testing.T) {
	noSleep(t)
	stubGhAuth(t, true)
	t.Setenv("PROMISE_BLOB_MIRROR", "")

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		fmt.Fprint(w, "from-http")
	}))
	defer srv.Close()

	dir := t.TempDir()
	dst := filepath.Join(dir, "abc.br")
	stubGhDownload(t, func(tag, asset, d string) error {
		return os.WriteFile(filepath.Join(d, asset), []byte("from-gh"), 0o644)
	})

	if err := fetchBlobRanked("deps-llvm-22.1.0", "abc.br", dst); err != nil {
		t.Fatalf("fetchBlobRanked: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "from-gh" {
		t.Errorf("content = %q, want the gh copy", got)
	}
	if hits != 0 {
		t.Errorf("HTTP sources hit %d times; gh should have satisfied the fetch", hits)
	}
}

// TestFetchBlobRanked_GhFailureFallsThrough covers the half-broken host: gh is
// installed and logged in but the download fails (revoked token, network blip,
// asset absent). The anonymous sources must still rescue the build.
func TestFetchBlobRanked_GhFailureFallsThrough(t *testing.T) {
	noSleep(t)
	stubGhAuth(t, true)
	stubGhDownload(t, func(tag, asset, dir string) error {
		return fmt.Errorf("exit status 1")
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "from-mirror")
	}))
	defer srv.Close()
	t.Setenv("PROMISE_BLOB_MIRROR", srv.URL)

	dst := filepath.Join(t.TempDir(), "abc.br")
	if err := fetchBlobRanked("deps-llvm-22.1.0", "abc.br", dst); err != nil {
		t.Fatalf("fetchBlobRanked: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "from-mirror" {
		t.Errorf("content = %q, want the mirror copy", got)
	}
}

// TestFetchBlobRanked_AllSourcesFailNamesEach pins the diagnostic: when every
// source fails the error must say which ones were tried and why, including the
// fact that gh was skipped for lack of credentials. A bare "exit status 1" is
// what made the original failure so hard to read.
func TestFetchBlobRanked_AllSourcesFailNamesEach(t *testing.T) {
	noSleep(t)
	stubGhAuth(t, false)
	failGhDownload(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	t.Setenv("PROMISE_BLOB_MIRROR", srv.URL)

	dst := filepath.Join(t.TempDir(), "abc.br")
	err := fetchBlobRanked("deps-llvm-22.1.0", "abc.br", dst)
	if err == nil {
		t.Fatal("expected an error when every source fails")
	}
	for _, want := range []string{"gh release download", "not authenticated", srv.URL, releaseAssetBase} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got:\n%v", want, err)
		}
	}
	if _, statErr := os.Stat(dst); statErr == nil {
		t.Error("a failed fetch must not leave a file at dst")
	}
}

// TestFetchHTTPSource_PermanentStatusNoRetry pins that a definitive 4xx costs one
// request. Retrying it would burn the whole backoff schedule per blob before
// reaching a source that works — the exact latency bug called out in #7.
func TestFetchHTTPSource_PermanentStatusNoRetry(t *testing.T) {
	noSleep(t)
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.NotFound(w, r)
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "abc.br")
	if err := fetchHTTPSource(srv.URL+"/abc.br", dst); err == nil {
		t.Fatal("expected an error on 404")
	}
	if hits != 1 {
		t.Errorf("404 produced %d requests, want exactly 1 (no retries)", hits)
	}
}

// TestFetchHTTPSource_RetriesTransient is the counterpart: a 5xx IS transient and
// must be retried rather than falling straight through to the next source.
func TestFetchHTTPSource_RetriesTransient(t *testing.T) {
	noSleep(t)
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, "ok-eventually")
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "abc.br")
	if err := fetchHTTPSource(srv.URL+"/abc.br", dst); err != nil {
		t.Fatalf("fetchHTTPSource: %v", err)
	}
	if hits != 3 {
		t.Errorf("got %d requests, want 3 (two 503s then success)", hits)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "ok-eventually" {
		t.Errorf("content = %q", got)
	}
}

// TestHTTPDownloadBlob_NoPartialOnFailure pins the temp-file+rename: a failed
// download must not leave a truncated blob at dst for the next ranked source or
// a later cache probe to mistake for a good one.
func TestHTTPDownloadBlob_NoPartialOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	dir := t.TempDir()
	dst := filepath.Join(dir, "abc.br")
	if err := httpDownloadBlob(srv.URL+"/abc.br", dst); err == nil {
		t.Fatal("expected an error on 403")
	}
	if _, err := os.Stat(dst); err == nil {
		t.Error("dst exists after a failed download")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".part-") {
			t.Errorf("leftover temp file %q", e.Name())
		}
	}
}
