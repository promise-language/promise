package common

import (
	"strings"
)

// openssl_slim.go is the OpenSSL counterpart of musl_slim.go (T1596 / #28). It lets
// `bin/build` obtain the musl-built static OpenSSL archives (libssl.a,
// libcrypto.a) from the hosted blob catalog — or, on a catalog miss, straight
// from the pinned upstream Alpine `openssl-libs-static` apk — instead of
// scraping them off the build host (which cannot work: the host ships glibc
// dynamic libssl only, and Promise links Linux binaries `-static` against
// musl). Everything here is a near-clone of the musl CRT machinery, which was
// already generic over the dependency name; only the dep string, arch names,
// and file list differ.
//
// **OpenSSL is a TARGET dependency, not a host one** — same axis as the musl
// CRT: a darwin host cross-compiling to linux-arm64 needs the linux-arm64
// OpenSSL archives, not its own. Callers pass a Linux target
// ("linux-amd64" / "linux-arm64"), never CurrentBuildTarget() unconditionally.
//
// **Nothing links OpenSSL yet.** This item wires the acquisition/embed/manifest
// paths end to end but the link gate (needsTLS) is never set true until T0077
// emits the TLS codegen bridge, so no build actually pulls these archives.

// OpenSSLTargets returns the Linux targets that carry static OpenSSL archives,
// in a stable order. See target_dep.go for the shared arch layout.
func OpenSSLTargets() []string { return LinuxTargetDepTargets() }

// OpenSSLArchDir returns the arch directory name for a Linux target (e.g.
// "linux-arm64" → "aarch64-linux-musl"), used by the compiler's OpenSSL layout
// (`resources/openssl/<arch>/`, `<PROMISE_HOME>/cache/openssl/<arch>/`). The
// SAME musl-arch triples as the CRT — the archives are, after all, the musl
// build of OpenSSL.
func OpenSSLArchDir(target string) (string, error) {
	return linuxTargetArchDir("static OpenSSL", target)
}

// OpenSSLManifestName is the runtime-manifest logical name for one OpenSSL
// archive on one arch, e.g. ("aarch64-linux-musl", "libssl.a") →
// "openssl-aarch64-linux-musl-libssl.a".
func OpenSSLManifestName(arch, file string) string {
	return targetDepManifestName("openssl", arch, file)
}

// EnsureOpenSSLBlobs returns a host-stable directory whose flat contents are the
// static OpenSSL archives (libssl.a, libcrypto.a per the prebuilts.toml `out`
// names) for the given LINUX target. Uses the catalog's brotli-11 slim blobs
// when present; falls back to the pinned upstream apk when blobs.json has no
// entry, printing a one-line `note:` so a maintainer can backfill via
// `bin/release publish-blobs --dependency openssl --host <target>`.
//
// Safe for concurrent invocation (per-target flock + content-addressed
// `tools.ok`), same as EnsureMuslBlobs. No post-fetch hook: the archives are
// inert relocatable ELF, never executed, so there is nothing to patch or sign.
func EnsureOpenSSLBlobs(root, target string) (string, error) {
	if _, err := OpenSSLArchDir(target); err != nil {
		return "", err
	}
	return ensureSlimBlobs(root, "openssl", target, "~8 MB", nil)
}

// openSSLPinned reports whether the OpenSSL prebuilt for `target` can actually be
// obtained today: either its sha256 is filled in prebuilts.toml (verified
// upstream apk) or the blob catalog hosts every one of its files. When neither
// is true (the near-term state — the pinning session had no network, so the
// sha256s are blank and no blobs are published), the build-host embed step skips
// the fetch entirely rather than attempting a doomed download on every build.
//
// This is what keeps offline / pre-pin `bin/build` green: OpenSSL is optional
// until TLS is wired (T0077), and a missing embed is invisible while no program
// links it. Once the sha256s are pinned OR the blobs are published, this flips
// true and EmbedOpenSSL starts embedding normally with no other change.
func openSSLPinned(root, target string) bool {
	pm, err := LoadPrebuiltsManifest(root)
	if err != nil || pm.Binaries["openssl"] == nil {
		return false
	}
	entry := pm.Binaries["openssl"]
	tEntry := entry.Targets[target]
	if tEntry == nil || tEntry.Unsupported != "" {
		return false
	}
	if strings.TrimSpace(tEntry.SHA256) != "" {
		return true
	}
	// No verified upstream sha256 — pinned only if the catalog can serve every
	// file (a maintainer ran `bin/release publish-blobs --dependency openssl`).
	catalog, err := LoadBlobsCatalog(root)
	if err != nil {
		return false
	}
	for _, f := range tEntry.Files {
		if _, ok := catalog.Lookup("openssl", entry.Version, target, f.Out); !ok {
			return false
		}
	}
	return true
}
