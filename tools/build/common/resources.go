package common

import (
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
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

	// winlink/ — self-generated Windows import libs (T0772). The .def symbol
	// lists under tools/build/winlink/def/ are the source of truth;
	// ensureWinlinkLibs regenerates the .lib in place into
	// resources/winlink/windows-amd64/ (this build-cleared embed tree) whenever a
	// .def is newer (T0835). Generated unconditionally (any host may
	// cross-compile a Windows target); go:embedded into the compiler binary.
	// llvm-dlltool is a build-only dependency — the client running promise needs
	// neither it nor the .def/.lib.
	if err := ensureWinlinkLibs(root); err != nil {
		return fmt.Errorf("generate winlink libs: %w", err)
	}

	// manifest.json — the always-embedded runtime dependency manifest (T0769).
	// Debug/thin builds get an empty-entries placeholder (host LLVM is resolved
	// from PATH/Homebrew). Release/full builds overwrite this with real entries
	// via GenerateRuntimeManifest after prebuilts are fetched.
	if err := ensureRuntimeManifest(res); err != nil {
		return fmt.Errorf("write manifest.json: %w", err)
	}

	return nil
}

// ensureRuntimeManifest (re)writes the empty placeholder runtime manifest so the
// `//go:embed resources/manifest.json` directive always resolves, and so a debug
// build after a release never embeds a stale release manifest. Release builds
// overwrite this afterward via GenerateRuntimeManifest (build.go step 6).
func ensureRuntimeManifest(resDir string) error {
	path := filepath.Join(resDir, "manifest.json")
	return os.WriteFile(path, []byte("{\n  \"schema\": 1,\n  \"epoch\": \"\",\n  \"entries\": []\n}\n"), 0o644)
}

// EmbedMuslCRT stages the musl C runtime objects into
// resources/crt/<musl-arch>/ for `//go:embed` (Linux hosts only — see
// compiler/cmd/promise/crt_linux_*.go; other hosts compile crt_other.go's empty
// stub and need nothing staged).
//
// The bytes come from the pinned prebuilt (hosted blob, or the upstream Alpine
// apk on a catalog miss) — never from the build host's /usr/lib. Scraping
// `/usr/lib/<arch>-linux-musl/` made the compiler's link output depend on
// whichever musl-dev the host happened to have installed, and broke `bin/build`
// outright on hosts with none (T0530).
//
// Only the HOST arch is staged: the embed directive is per-GOARCH, and
// cross-arch Linux linking resolves its CRT from the content-addressed store at
// link time (the arch-qualified `musl-<arch>-*` manifest entries) rather than
// doubling every Linux binary's embedded libc.a.
func EmbedMuslCRT(root string) error {
	if !IsLinux() {
		return nil
	}
	target := CurrentBuildTarget()
	muslArch, err := MuslArchDir(target)
	if err != nil {
		return err
	}
	src, err := EnsureMuslBlobs(root, target)
	if err != nil {
		return fmt.Errorf("fetch musl CRT for %s: %w", target, err)
	}
	pm, err := LoadPrebuiltsManifest(root)
	if err != nil {
		return err
	}
	tEntry := pm.Binaries["musl"].Targets[target]

	// Wipe first so a file dropped from prebuilts.toml can't linger and get
	// swept back in by the embed glob (mirrors wipeBundleDir for LLVM).
	dst := filepath.Join(root, "compiler", "cmd", "promise", "resources", "crt", muslArch)
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, f := range tEntry.ClientFiles() {
		if err := copyFile(filepath.Join(src, f.Out), filepath.Join(dst, f.Out)); err != nil {
			return fmt.Errorf("stage musl %s: %w", f.Out, err)
		}
	}
	return nil
}

// openSSLPlaceholderName is the sentinel file EmbedOpenSSL drops into an arch
// dir when the real archives can't be obtained (offline / not-yet-pinned). Its
// only purpose is to give `//go:embed resources/openssl/<arch>/*` at least one
// matching file so the compiler package still builds — the `*` glob skips
// dot-prefixed names, so this is deliberately NOT dot-prefixed. Consumers key
// availability on the actual `libssl.a`/`libcrypto.a` presence (openSSLComplete
// in main.go), so a dir holding only this sentinel reads as "OpenSSL not
// available", which is correct while TLS is unwired.
const openSSLPlaceholderName = "PLACEHOLDER"

// EmbedOpenSSL stages the musl-static OpenSSL archives (libssl.a, libcrypto.a)
// into resources/openssl/<musl-arch>/ for `//go:embed` (Linux hosts only — see
// compiler/cmd/promise/openssl_linux_*.go; other hosts compile
// openssl_other.go's empty stub and need nothing staged).
//
// This mirrors EmbedMuslCRT but is BEST-EFFORT rather than fatal: OpenSSL is
// only needed to link TLS programs, and no program links TLS yet (T1596 / #28
// introduces the link gate but never sets it — T0077 will). So a build host
// that cannot obtain the archives (they are not pinned/hosted yet, or it is
// offline) must NOT fail the whole build; it stages a placeholder so the embed
// directive still resolves and moves on. TLS links, once they exist, resolve
// OpenSSL from the content-addressed store / on-demand fetch instead.
//
// When the thin/full release split lands (docs/distribution.md §1, §4) the embed
// decision becomes a release-flavour knob (full = embed, thin = fetch-on-demand)
// rather than always-embed; until then this defaults to embedding, matching
// today's `--release` == "full".
//
// Only the HOST arch is staged (the embed directive is per-GOARCH); cross-arch
// Linux TLS linking resolves its archives from the content-addressed store.
func EmbedOpenSSL(root string) error {
	if !IsLinux() {
		return nil
	}
	target := CurrentBuildTarget()
	arch, err := OpenSSLArchDir(target)
	if err != nil {
		// Host is Linux but on an arch we don't carry OpenSSL for — nothing to
		// embed. (Can't happen for amd64/arm64; a guard, not a real path.)
		return nil
	}

	// Wipe first so a stale archive can't linger and get swept back in by the
	// embed glob (mirrors EmbedMuslCRT).
	dst := filepath.Join(root, "compiler", "cmd", "promise", "resources", "openssl", arch)
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}

	if !openSSLPinned(root, target) {
		fmt.Println("note: OpenSSL archives not pinned/hosted yet — skipping embed (TLS support pending T0077); no program links TLS, so this is a no-op for now")
		return writeOpenSSLPlaceholder(dst)
	}

	src, err := EnsureOpenSSLBlobs(root, target)
	if err != nil {
		fmt.Printf("note: could not obtain OpenSSL archives for %s (%v) — skipping embed; TLS links (none yet) would fetch on demand\n", target, err)
		return writeOpenSSLPlaceholder(dst)
	}
	pm, err := LoadPrebuiltsManifest(root)
	if err != nil {
		return err
	}
	tEntry := pm.Binaries["openssl"].Targets[target]
	for _, f := range tEntry.ClientFiles() {
		if err := copyFile(filepath.Join(src, f.Out), filepath.Join(dst, f.Out)); err != nil {
			return fmt.Errorf("stage openssl %s: %w", f.Out, err)
		}
	}
	return nil
}

// writeOpenSSLPlaceholder drops the sentinel that keeps the go:embed directive
// satisfiable when the real archives aren't available. See
// openSSLPlaceholderName.
func writeOpenSSLPlaceholder(dstDir string) error {
	msg := "OpenSSL static archives are not embedded in this build.\n" +
		"See #28 / prebuilts.toml [binaries.openssl]. TLS links resolve\n" +
		"libssl.a/libcrypto.a from the content-addressed store on demand.\n"
	return os.WriteFile(filepath.Join(dstDir, openSSLPlaceholderName), []byte(msg), 0o644)
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
