package blobstore

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// extractArchive extracts a tar-family archive (plain/.gz/.xz/.zst, via system
// tar which autodetects compression) or a .zip (stdlib) into dst. The format is
// detected from the file's magic bytes rather than its name, because CAS-cached
// archives live at archives/sha256/<hash> with no extension. A runtime-side copy
// of tools/build/common.ExtractArchive — that package is build-only and not
// importable into the shipped binary.
func extractArchive(archive, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	if isZip(archive) {
		return extractZip(archive, dst)
	}
	// Everything else: hand to system tar, which autodetects gz/xz/zst/plain.
	cmd := exec.Command("tar", "-xf", archive, "-C", dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tar -xf %s: %v\n%s", archive, err, out)
	}
	return nil
}

// isZip reports whether the file begins with the ZIP local-file-header magic.
func isZip(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return false
	}
	return magic == [4]byte{'P', 'K', 0x03, 0x04} || magic == [4]byte{'P', 'K', 0x05, 0x06}
}

func extractZip(archive, dst string) error {
	r, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		name := filepath.Clean(f.Name)
		if strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			return fmt.Errorf("zip: refusing to extract escaping path %q", f.Name)
		}
		path := filepath.Join(dst, name)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(path, f.Mode()); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			rc.Close()
			return err
		}
		if _, err := io.Copy(out, rc); err != nil {
			rc.Close()
			out.Close()
			return err
		}
		rc.Close()
		if err := out.Close(); err != nil {
			return err
		}
	}
	return nil
}
