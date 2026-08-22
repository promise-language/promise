package common

// musl_slim.go is the musl CRT counterpart of llvm_slim.go (T0530). It lets
// `bin/build` obtain the static-link CRT objects (crt1.o, crti.o, crtn.o,
// libc.a) from the hosted blob catalog — or, on a catalog miss, straight from
// the pinned upstream Alpine `musl-dev` apk — instead of scraping them off the
// build host's /usr/lib.
//
// Before this, EmbedMuslCRT copied out of `/usr/lib/<arch>-linux-musl/`, so
// `bin/build` hard-required a preinstalled `musl-dev` and failed outright on any
// host without one (e.g. an arm64 Alpine dev container). The CRT is now a
// prebuilt like every other dependency: pinned in prebuilts.toml, hashed by
// `bin/pin-prebuilts`, hosted content-addressed by `bin/release publish-blobs`.
//
// **The CRT is a TARGET dependency, not a host one.** Promise links Linux
// binaries `-static` against musl, so the relevant axis is the Linux target
// being linked FOR — a darwin host cross-compiling to linux-arm64 needs the
// linux-arm64 CRT, not its own. Callers therefore pass a Linux target
// ("linux-amd64" / "linux-arm64"), never CurrentBuildTarget() unconditionally.

// MuslTargets returns the Linux targets that carry a musl CRT, in a stable
// order. See target_dep.go for the shared arch layout every per-arch target
// dependency uses.
func MuslTargets() []string { return LinuxTargetDepTargets() }

// MuslArchDir returns the musl arch directory name for a Linux target (e.g.
// "linux-arm64" → "aarch64-linux-musl"), used by the compiler's CRT layout
// (`resources/crt/<arch>/`, `<PROMISE_HOME>/cache/crt/<arch>/`). The names are
// the GNU-style triples the runtime's muslArchDir() in
// compiler/cmd/promise/main.go already produces — keep the two in sync.
func MuslArchDir(target string) (string, error) {
	return linuxTargetArchDir("musl CRT", target)
}

// MuslManifestName is the runtime-manifest logical name for one musl CRT object
// on one arch, e.g. ("aarch64-linux-musl", "crt1.o") →
// "musl-aarch64-linux-musl-crt1.o".
func MuslManifestName(arch, file string) string { return targetDepManifestName("musl", arch, file) }

// EnsureMuslBlobs returns a host-stable directory whose flat contents are the
// musl CRT objects (crt1.o, crti.o, crtn.o, libc.a per the prebuilts.toml `out`
// names) for the given LINUX target. Uses the catalog's brotli-11 slim blobs
// when present; falls back to the pinned upstream apk when blobs.json has no
// entry, printing a one-line `note:` so a maintainer can backfill via
// `bin/release publish-blobs --dependency musl --host <target>`.
//
// Safe for concurrent invocation (per-target flock + content-addressed
// `tools.ok`), same as EnsureLLVMBlobs. No post-fetch hook: CRT objects are
// inert relocatable ELF, never executed, so there is nothing to patch or sign.
func EnsureMuslBlobs(root, target string) (string, error) {
	if _, err := MuslArchDir(target); err != nil {
		return "", err
	}
	return ensureSlimBlobs(root, "musl", target, "~2.4 MB", nil)
}
