package common

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// apk_test.go covers extractAPK — the Alpine package reader behind the musl CRT
// prebuilt (T0530). The interesting property is the archive shape: an apk is
// several concatenated gzip streams, and the leading segments are normally
// written WITHOUT tar's end-of-archive marker. Both that layout and the
// defensive "every segment terminated" one must yield the payload.

// apkSegment builds one gzip-compressed tar segment. When terminated is false
// the tar end-of-archive marker is omitted — the "cut short" form real apk
// signature/control segments use, which is what lets a single tar walk run
// straight through the concatenation.
func apkSegment(t *testing.T, files map[string]string, terminated bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic member order

	for _, name := range names {
		body := files[name]
		if err := tw.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if terminated {
		if err := tw.Close(); err != nil { // writes the end-of-archive marker
			t.Fatal(err)
		}
	} else {
		if err := tw.Flush(); err != nil { // pads the last member, no marker
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// writeAPK concatenates segments into a .apk file under a temp dir and returns
// its path.
func writeAPK(t *testing.T, segments ...[]byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pkg.apk")
	var all []byte
	for _, s := range segments {
		all = append(all, s...)
	}
	if err := os.WriteFile(path, all, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// muslDevAPK builds a stand-in for Alpine's musl-dev package: a cut-short
// signature segment, a cut-short control segment, and a terminated data segment
// carrying the CRT payload at the paths prebuilts.toml declares.
func muslDevAPK(t *testing.T) string {
	t.Helper()
	return writeAPK(t,
		apkSegment(t, map[string]string{".SIGN.RSA.test.pub": "signature"}, false),
		apkSegment(t, map[string]string{".PKGINFO": "pkgname = musl-dev\n"}, false),
		apkSegment(t, map[string]string{
			"usr/lib/crt1.o": "CRT1",
			"usr/lib/crti.o": "CRTI",
			"usr/lib/crtn.o": "CRTN",
			"usr/lib/libc.a": "LIBC",
		}, true),
	)
}

func TestExtractAPK_ConcatenatedSegments(t *testing.T) {
	dst := t.TempDir()
	if err := extractAPK(muslDevAPK(t), dst); err != nil {
		t.Fatalf("extractAPK: %v", err)
	}
	for name, want := range map[string]string{
		"usr/lib/crt1.o": "CRT1",
		"usr/lib/crti.o": "CRTI",
		"usr/lib/crtn.o": "CRTN",
		"usr/lib/libc.a": "LIBC",
	} {
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	// Metadata members from the leading segments must survive the walk too —
	// their presence is what proves the reader didn't stop at segment 1.
	if !Exists(filepath.Join(dst, ".PKGINFO")) {
		t.Error(".PKGINFO missing — the control segment was not read")
	}
}

// TestExtractAPK_AllSegmentsTerminated covers the defensive branch: if a
// producer writes every segment with its end-of-archive marker, archive/tar
// reports EOF after the FIRST one. The reader must restart on a fresh
// tar.Reader and still deliver the payload rather than silently extracting
// nothing (which would surface far away as "manifest probe not found").
func TestExtractAPK_AllSegmentsTerminated(t *testing.T) {
	path := writeAPK(t,
		apkSegment(t, map[string]string{".SIGN.RSA.test.pub": "signature"}, true),
		apkSegment(t, map[string]string{".PKGINFO": "pkgname = musl-dev\n"}, true),
		apkSegment(t, map[string]string{"usr/lib/crt1.o": "CRT1"}, true),
	)
	dst := t.TempDir()
	if err := extractAPK(path, dst); err != nil {
		t.Fatalf("extractAPK: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "usr", "lib", "crt1.o"))
	if err != nil {
		t.Fatalf("payload from the third terminated segment is missing: %v", err)
	}
	if string(got) != "CRT1" {
		t.Errorf("crt1.o = %q, want CRT1", got)
	}
}

// TestExtractAPK_SingleSegment is the degenerate case — a plain gzipped tar with
// no metadata segments at all.
func TestExtractAPK_SingleSegment(t *testing.T) {
	path := writeAPK(t, apkSegment(t, map[string]string{"usr/lib/libc.a": "LIBC"}, true))
	dst := t.TempDir()
	if err := extractAPK(path, dst); err != nil {
		t.Fatalf("extractAPK: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "usr", "lib", "libc.a")); err != nil || string(got) != "LIBC" {
		t.Errorf("libc.a = %q (err=%v), want LIBC", got, err)
	}
}

func TestExtractAPK_RejectsEscapingPath(t *testing.T) {
	path := writeAPK(t, apkSegment(t, map[string]string{"../escaped.o": "EVIL"}, true))
	dst := t.TempDir()
	err := extractAPK(path, dst)
	if err == nil {
		t.Fatal("expected extractAPK to reject a path escaping the extract dir")
	}
	if !strings.Contains(err.Error(), "escaping path") {
		t.Errorf("error should name the escaping path, got: %v", err)
	}
	if Exists(filepath.Join(filepath.Dir(dst), "escaped.o")) {
		t.Error("escaping member was written outside the extract dir")
	}
}

func TestExtractAPK_RejectsAbsolutePath(t *testing.T) {
	path := writeAPK(t, apkSegment(t, map[string]string{"/etc/passwd": "EVIL"}, true))
	if err := extractAPK(path, t.TempDir()); err == nil {
		t.Fatal("expected extractAPK to reject an absolute member path")
	}
}

func TestExtractAPK_NotGzip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.apk")
	if err := os.WriteFile(path, []byte("this is not a gzip stream"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := extractAPK(path, t.TempDir())
	if err == nil {
		t.Fatal("expected extractAPK to reject non-gzip input")
	}
	if !strings.Contains(err.Error(), "gzip") {
		t.Errorf("error should mention gzip, got: %v", err)
	}
}

// TestExtractArchive_DispatchesAPK pins the dispatch: `.apk` must route to the
// stdlib reader, NOT to the zip branch (Android shares the extension) and not to
// the `tar` subprocess (whose behaviour on concatenated streams differs across
// GNU/BSD/busybox — the reason this reader exists).
func TestExtractArchive_DispatchesAPK(t *testing.T) {
	dst := t.TempDir()
	if err := ExtractArchive(muslDevAPK(t), dst); err != nil {
		t.Fatalf("ExtractArchive(.apk): %v", err)
	}
	if !Exists(filepath.Join(dst, "usr", "lib", "crt1.o")) {
		t.Error("ExtractArchive did not route .apk to extractAPK")
	}
}

func TestArchiveExtFor_APK(t *testing.T) {
	url := "https://dl-cdn.alpinelinux.org/alpine/v3.23/main/aarch64/musl-dev-1.2.5-r23.apk"
	if got := archiveExtFor(url); got != ".apk" {
		t.Errorf("archiveExtFor(%q) = %q, want .apk (a .tar.xz default would name the cache file wrong)", url, got)
	}
}

// TestResolveInnerRoot_APKLayout pins the interaction between the apk's flat
// `usr/lib/...` layout and the manifest probe: the extracted tree has no single
// wrapper dir, so resolveInnerRoot must select tmpRoot itself.
func TestResolveInnerRoot_APKLayout(t *testing.T) {
	dst := t.TempDir()
	if err := extractAPK(muslDevAPK(t), dst); err != nil {
		t.Fatal(err)
	}
	got, err := resolveInnerRoot(dst, []PrebuiltFile{{Src: "usr/lib/crt1.o", Out: "crt1.o"}})
	if err != nil {
		t.Fatalf("resolveInnerRoot: %v", err)
	}
	if got != dst {
		t.Errorf("innerRoot = %q, want the flat extract root %q", got, dst)
	}
}
