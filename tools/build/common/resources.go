package common

import (
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// EmbedResources copies project files into compiler/cmd/promise/resources/
// for Go's embed directive, and updates testdata.
func EmbedResources(root string) error {
	res := filepath.Join(root, "compiler", "cmd", "promise", "resources")
	if err := os.MkdirAll(res, 0o755); err != nil {
		return err
	}

	// catalog.toml
	if err := copyFile(filepath.Join(root, "catalog.toml"), filepath.Join(res, "catalog.toml")); err != nil {
		return fmt.Errorf("copy catalog.toml: %w", err)
	}

	// language-guide.md
	if err := copyFile(filepath.Join(root, "docs", "language-guide.md"), filepath.Join(res, "language-guide.md")); err != nil {
		return fmt.Errorf("copy language-guide.md: %w", err)
	}

	// modules/ (clean copy)
	modulesRes := filepath.Join(res, "modules")
	os.RemoveAll(modulesRes)
	if err := os.MkdirAll(modulesRes, 0o755); err != nil {
		return err
	}
	// Touch .keep for go:embed
	os.WriteFile(filepath.Join(modulesRes, ".keep"), nil, 0o644)

	modulesDir := filepath.Join(root, "modules")
	if Exists(modulesDir) {
		entries, err := os.ReadDir(modulesDir)
		if err != nil {
			return fmt.Errorf("read modules/: %w", err)
		}
		for _, e := range entries {
			if e.IsDir() {
				src := filepath.Join(modulesDir, e.Name())
				dst := filepath.Join(modulesRes, e.Name())
				if err := copyDir(src, dst); err != nil {
					return fmt.Errorf("copy modules/%s: %w", e.Name(), err)
				}
			}
		}
	}

	// testdata/std (for Go tests)
	testdataStd := filepath.Join(root, "compiler", "internal", "testutil", "testdata", "std")
	os.RemoveAll(testdataStd)
	if err := os.MkdirAll(testdataStd, 0o755); err != nil {
		return err
	}
	stdDir := filepath.Join(root, "modules", "std")
	if Exists(stdDir) {
		entries, err := os.ReadDir(stdDir)
		if err != nil {
			return fmt.Errorf("read modules/std/: %w", err)
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".pr") {
				if err := copyFile(filepath.Join(stdDir, e.Name()), filepath.Join(testdataStd, e.Name())); err != nil {
					return fmt.Errorf("copy std/%s: %w", e.Name(), err)
				}
			}
		}
	}

	// examples/ (clean copy, remove README.md)
	examplesRes := filepath.Join(res, "examples")
	os.RemoveAll(examplesRes)
	if err := copyDir(filepath.Join(root, "examples"), examplesRes); err != nil {
		return fmt.Errorf("copy examples/: %w", err)
	}
	os.Remove(filepath.Join(examplesRes, "README.md"))

	// .sources.sha256
	if err := computeSourcesSHA256(root, res); err != nil {
		return fmt.Errorf("compute .sources.sha256: %w", err)
	}

	return nil
}

// EmbedMuslCRT copies musl C runtime objects (Linux only).
func EmbedMuslCRT(root string) error {
	if !IsLinux() {
		return nil
	}
	muslArch := "x86_64-linux-musl"
	if runtime.GOARCH == "arm64" {
		muslArch = "aarch64-linux-musl"
	}
	src := "/usr/lib/" + muslArch
	dst := filepath.Join(root, "compiler", "cmd", "promise", "resources", "crt", muslArch)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, name := range []string{"crt1.o", "crti.o", "crtn.o", "libc.a"} {
		if err := copyFile(filepath.Join(src, name), filepath.Join(dst, name)); err != nil {
			return fmt.Errorf("copy musl %s: %w", name, err)
		}
	}
	return nil
}

// computeSourcesSHA256 generates .sources.sha256 matching the Makefile's format:
// (cd .. && find modules/ catalog.toml -type f | sort | xargs sha256sum)
func computeSourcesSHA256(root, resDir string) error {
	var files []string

	// Walk modules/
	modulesDir := filepath.Join(root, "modules")
	if Exists(modulesDir) {
		if err := filepath.WalkDir(modulesDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			// Use forward slashes to match sha256sum output format (cross-platform)
			files = append(files, filepath.ToSlash(rel))
			return nil
		}); err != nil {
			return err
		}
	}

	// catalog.toml
	files = append(files, "catalog.toml")
	sort.Strings(files)

	var lines []string
	for _, rel := range files {
		abs := filepath.Join(root, rel)
		h, err := fileSHA256(abs)
		if err != nil {
			return fmt.Errorf("hash %s: %w", rel, err)
		}
		// Match sha256sum format: "<hash>  <path>"  (two spaces)
		lines = append(lines, fmt.Sprintf("%s  %s", h, rel))
	}

	out := filepath.Join(resDir, ".sources.sha256")
	return os.WriteFile(out, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	})
}
