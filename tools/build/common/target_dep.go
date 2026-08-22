package common

import "fmt"

// target_dep.go holds the machinery shared by every *per-arch Linux target
// dependency* — the musl CRT (T0530), the static OpenSSL archives (T1596), and
// the compiler-rt builtins archive (T1676).
//
// All three are the same shape: a flat file set, sliced from a pinned Alpine
// apk, staged under a musl-arch directory (`resources/<dep>/<arch>/`,
// `<PROMISE_HOME>/cache/<dep>/<arch>/`), hosted content-addressed by
// `bin/release publish-blobs --dependency <dep>`, and projected into the runtime
// manifest under an arch-qualified logical name. They are TARGET dependencies,
// not host ones: a darwin host cross-compiling to linux-arm64 needs the
// linux-arm64 files, not its own — so callers pass a Linux target
// ("linux-amd64" / "linux-arm64"), never CurrentBuildTarget() unconditionally.
//
// What differs between them is policy, not mechanism: the musl CRT and
// compiler-rt builtins are REQUIRED (every Linux link line carries them, so a
// build that cannot obtain them must fail loudly), while OpenSSL is
// best-effort (only TLS programs link it). That difference lives in each
// dependency's Ensure*/Embed* wrapper, not here.

// linuxTargetArchDirs maps a prebuilts.toml Linux target to the musl arch
// directory name every target dependency stages under. One map for all of them:
// the names are the GNU-style triples the runtime's muslArchDir() in
// compiler/cmd/promise/main.go produces, and OpenSSL / compiler-rt deliberately
// reuse them (they are, after all, the musl builds of those projects) so the
// on-disk layout is uniform. Keep the two sides in sync.
var linuxTargetArchDirs = map[string]string{
	"linux-amd64": "x86_64-linux-musl",
	"linux-arm64": "aarch64-linux-musl",
}

// LinuxTargetDepTargets returns the Linux targets that carry per-arch target
// dependencies, in a stable order. Adding a target means touching
// linuxTargetArchDirs (and prebuilts.toml) in one place.
func LinuxTargetDepTargets() []string { return []string{"linux-amd64", "linux-arm64"} }

// linuxTargetArchDir resolves the arch directory name for a Linux target (e.g.
// "linux-arm64" → "aarch64-linux-musl"). A non-Linux or unknown target is an
// error rather than a silent default: guessing here would stage one arch's
// files under another arch's name and produce link failures far from the cause.
// `what` names the dependency in the error message.
func linuxTargetArchDir(what, target string) (string, error) {
	arch, ok := linuxTargetArchDirs[target]
	if !ok {
		return "", fmt.Errorf("no %s arch for target %q (want one of linux-amd64, linux-arm64)", what, target)
	}
	return arch, nil
}

// targetDepManifestName is the runtime-manifest logical name for one file of a
// target dependency on one arch, e.g. ("musl", "aarch64-linux-musl", "crt1.o")
// → "musl-aarch64-linux-musl-crt1.o".
//
// The arch is part of the NAME, not just the catalog's (dep, version, target,
// name) identity, because these are target dependencies: one host manifest can
// carry several arches at once, and an unqualified "musl-crt1.o" could only
// ever describe one of them. Keep in lockstep with targetDepManifestName in
// compiler/cmd/promise/llvm_cas.go — separate Go modules, so the format is
// duplicated by necessity (pinned by TestMuslManifestName,
// TestOpenSSLManifestName, TestCompilerRTManifestName on both sides).
func targetDepManifestName(dep, arch, file string) string {
	return dep + "-" + arch + "-" + file
}
