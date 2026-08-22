package common

// compiler_rt_slim.go is the compiler-rt counterpart of musl_slim.go /
// openssl_slim.go (T1676). It lets `bin/build` obtain the musl-built
// compiler-rt builtins archive (libclang_rt.builtins.a) from the hosted blob
// catalog — or, on a catalog miss, straight from the pinned upstream Alpine
// `compiler-rt` apk. Everything here is a near-clone of the musl CRT
// machinery, which was already generic over the dependency name; only the dep
// string and file list differ.
//
// **The builtins archive is a TARGET dependency, not a host one** — same axis
// as the musl CRT and OpenSSL: a darwin host cross-compiling to linux-arm64
// needs the linux-arm64 builtins, not its own. Callers pass a Linux target
// ("linux-amd64" / "linux-arm64"), never CurrentBuildTarget() unconditionally.
//
// **This one is REQUIRED, not best-effort.** Unlike OpenSSL (needed only by TLS
// programs), the builtins archive is spliced onto EVERY musl link line, because
// musl's own libc.a references the soft-float binary128 helpers on aarch64. It
// therefore fails loudly like EnsureMuslBlobs rather than degrading quietly
// like EnsureOpenSSLBlobs — see prebuilts.toml [binaries.compiler-rt].

// CompilerRTTargets returns the Linux targets that carry a compiler-rt builtins
// archive, in a stable order. See target_dep.go for the shared arch layout.
func CompilerRTTargets() []string { return LinuxTargetDepTargets() }

// CompilerRTArchDir returns the arch directory name for a Linux target (e.g.
// "linux-arm64" → "aarch64-linux-musl"), used by the compiler's builtins layout
// (`resources/compiler-rt/<arch>/`,
// `<PROMISE_HOME>/cache/compiler-rt/<arch>/`). The SAME musl-arch triples as
// the CRT and OpenSSL — the archive is, after all, the musl build of
// compiler-rt.
func CompilerRTArchDir(target string) (string, error) {
	return linuxTargetArchDir("compiler-rt builtins", target)
}

// CompilerRTManifestName is the runtime-manifest logical name for the builtins
// archive on one arch, e.g. ("aarch64-linux-musl", "libclang_rt.builtins.a") →
// "compiler-rt-aarch64-linux-musl-libclang_rt.builtins.a".
func CompilerRTManifestName(arch, file string) string {
	return targetDepManifestName("compiler-rt", arch, file)
}

// EnsureCompilerRTBlobs returns a host-stable directory whose flat contents are
// the compiler-rt builtins archive (libclang_rt.builtins.a per the
// prebuilts.toml `out` name) for the given LINUX target. Uses the catalog's
// brotli-11 slim blobs when present; falls back to the pinned upstream apk when
// blobs.json has no entry, printing a one-line `note:` so a maintainer can
// backfill via `bin/release publish-blobs --dependency compiler-rt --host
// <target>`.
//
// Safe for concurrent invocation (per-target flock + content-addressed
// `tools.ok`), same as EnsureMuslBlobs. No post-fetch hook: the archive is inert
// relocatable ELF, never executed, so there is nothing to patch or sign.
func EnsureCompilerRTBlobs(root, target string) (string, error) {
	if _, err := CompilerRTArchDir(target); err != nil {
		return "", err
	}
	return ensureSlimBlobs(root, "compiler-rt", target, "~12.5 MB", nil)
}
