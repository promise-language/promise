package common

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// apk.go extracts Alpine Linux `.apk` packages — the upstream archive format for
// the musl CRT prebuilt (T0530). An Alpine apk (v2) is NOT a zip despite sharing
// the extension with Android packages: it is three **concatenated gzip streams**
// (signature segment, control segment, data segment), where the first two carry
// tar members without an end-of-archive marker. Decompressing the concatenation
// as one gzip multistream therefore yields one continuous tar byte stream whose
// members are `.SIGN.*`, `.PKGINFO`, then the package payload (`usr/lib/crt1.o`,
// …). This is why `tar -xzf pkg.apk` works on GNU tar.
//
// We do it in-process with the stdlib instead of shelling out to `tar` because
// blob publishing runs on a maintainer's macOS box (bsdtar), CI (GNU tar) and
// Alpine dev containers (busybox tar), and those three disagree about warnings,
// exit codes, and lone-zero-block handling on concatenated streams. The stdlib
// path behaves identically everywhere.

// extractAPK extracts an Alpine `.apk` package into dst, flattening the
// concatenated gzip segments into one walk. Metadata members (`.SIGN.*`,
// `.PKGINFO`) land in dst alongside the payload and are simply ignored by the
// caller's `src` lookups.
func extractAPK(archive, dst string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()

	// Multistream(true) is the gzip default and is exactly what reads the
	// signature+control+data segment concatenation as one byte stream.
	zr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer zr.Close()

	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}

	// The signature and control segments are normally "cut short" — written
	// without tar's end-of-archive marker — which is why one tar walk sees all
	// three segments (and why `tar -xzf pkg.apk` works). Don't rely on it: if a
	// segment DOES carry its marker, archive/tar reports EOF there and the
	// payload would silently vanish. So restart on a fresh tar.Reader after each
	// end-of-archive and stop only when a pass yields no members — which also
	// terminates on the trailing zero padding that follows a well-formed marker.
	for {
		n, err := extractTarStream(tar.NewReader(zr), dst)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
	}
}

// extractTarStream writes every member of one tar archive into dst and returns
// how many it wrote. A zero count with a nil error means the archive was empty —
// the signal extractAPK uses to stop walking concatenated segments.
func extractTarStream(tr *tar.Reader, dst string) (int, error) {
	count := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return count, nil
		}
		if err != nil {
			// A truncated final segment is indistinguishable from corruption at
			// this layer; both are real errors for a sha256-verified archive.
			return count, fmt.Errorf("tar: %w", err)
		}
		count++
		if err := writeTarMember(hdr, tr, dst); err != nil {
			return count, err
		}
	}
}

// writeTarMember materializes one tar member under dst.
func writeTarMember(hdr *tar.Header, tr *tar.Reader, dst string) error {
	target, err := safeArchivePath(dst, hdr.Name)
	if err != nil {
		return err
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		return os.MkdirAll(target, hdr.FileInfo().Mode().Perm())
	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, hdr.FileInfo().Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	case tar.TypeSymlink:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		// A symlink target that escapes dst is only dangerous once followed, and
		// PrebuiltFile.ResolveSymlink is the only follower — it resolves against
		// the extract dir — so reject escaping links up front.
		if _, err := safeArchivePath(dst, filepath.Join(filepath.Dir(hdr.Name), hdr.Linkname)); err != nil {
			return fmt.Errorf("apk: refusing escaping symlink %q -> %q", hdr.Name, hdr.Linkname)
		}
		os.Remove(target)
		return os.Symlink(hdr.Linkname, target)
	case tar.TypeLink:
		source, err := safeArchivePath(dst, hdr.Linkname)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		os.Remove(target)
		return os.Link(source, target)
	default:
		// Character/block devices, FIFOs, and the pax/GNU extension headers the
		// stdlib already folded into hdr — nothing a CRT payload needs.
		return nil
	}
}

// safeArchivePath joins an archive member name onto dst, rejecting any name that
// would escape dst. Mirrors extractZip's guard: filepath.IsAbs alone is
// insufficient on Windows, where a unix-absolute "/etc/passwd" is not IsAbs yet
// Clean yields the rooted "\etc\passwd".
func safeArchivePath(dst, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == "" || clean == "." {
		return "", fmt.Errorf("apk: refusing empty member name %q", name)
	}
	if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) || os.IsPathSeparator(clean[0]) {
		return "", fmt.Errorf("apk: refusing to extract escaping path %q", name)
	}
	return filepath.Join(dst, clean), nil
}
