package common

import (
	"testing"
)

// target_dep_test.go pins the invariants of the machinery shared by every
// per-arch Linux target dependency (T1676). The musl CRT, static OpenSSL and
// compiler-rt builtins were three near-identical copies before T1676 collapsed
// them onto target_dep.go; these tests pin the property that made the collapse
// legitimate — all three really do use ONE target list and ONE arch layout — so
// a future divergence is a test failure rather than an arch-mismatched stage
// that only surfaces as a link error on one platform's CI.

// TestLinuxTargetDepTargetsCoverArchDirs pins that the target list and the arch
// map agree in both directions. A target listed but unmapped would make
// EnsureCompilerRTBlobs/EnsureMuslBlobs fail for a target the build tools claim
// to support; a mapped-but-unlisted arch would silently never be published.
func TestLinuxTargetDepTargetsCoverArchDirs(t *testing.T) {
	targets := LinuxTargetDepTargets()
	if len(targets) != len(linuxTargetArchDirs) {
		t.Errorf("LinuxTargetDepTargets() has %d entries, linuxTargetArchDirs has %d: %v vs %v",
			len(targets), len(linuxTargetArchDirs), targets, linuxTargetArchDirs)
	}
	seen := map[string]bool{}
	for _, target := range targets {
		if seen[target] {
			t.Errorf("duplicate target %q", target)
		}
		seen[target] = true
		arch, err := linuxTargetArchDir("test dep", target)
		if err != nil {
			t.Errorf("LinuxTargetDepTargets() lists %q but linuxTargetArchDir rejects it: %v", target, err)
		}
		if arch == "" {
			t.Errorf("target %q maps to an empty arch dir", target)
		}
	}
	for target := range linuxTargetArchDirs {
		if !seen[target] {
			t.Errorf("linuxTargetArchDirs maps %q but LinuxTargetDepTargets() omits it — it would never be published", target)
		}
	}
}

// TestTargetDepsShareOneLayout is the invariant target_dep.go exists to hold:
// the three dependencies expose the same target list and resolve every target
// to the SAME arch directory. They are staged side by side under one
// <PROMISE_HOME>/cache tree, so a divergence would put one dependency's files
// under a name another dependency's resolver reads.
func TestTargetDepsShareOneLayout(t *testing.T) {
	families := map[string]struct {
		targets []string
		archDir func(string) (string, error)
	}{
		"musl":        {MuslTargets(), MuslArchDir},
		"openssl":     {OpenSSLTargets(), OpenSSLArchDir},
		"compiler-rt": {CompilerRTTargets(), CompilerRTArchDir},
	}
	want := LinuxTargetDepTargets()
	for name, fam := range families {
		if len(fam.targets) != len(want) {
			t.Errorf("%s: targets %v, want %v", name, fam.targets, want)
			continue
		}
		for i, target := range want {
			if fam.targets[i] != target {
				t.Errorf("%s: targets[%d] = %q, want %q (order must be stable across deps)", name, i, fam.targets[i], target)
			}
			got, err := fam.archDir(target)
			if err != nil {
				t.Errorf("%s: archDir(%q): %v", name, target, err)
				continue
			}
			if got != linuxTargetArchDirs[target] {
				t.Errorf("%s: archDir(%q) = %q, want the shared %q", name, target, got, linuxTargetArchDirs[target])
			}
		}
		if _, err := fam.archDir("darwin-arm64"); err == nil {
			t.Errorf("%s: archDir(darwin-arm64) must fail rather than default to an arch", name)
		}
		if _, err := fam.archDir(""); err == nil {
			t.Errorf("%s: archDir(\"\") must fail", name)
		}
	}
}

// TestLinuxTargetArchDirErrorNamesDependency pins that the shared resolver
// still says WHICH dependency failed. The whole point of the `what` parameter
// is that "no arch for target X" from a shared helper is useless to a
// maintainer staring at a cross-compile failure.
func TestLinuxTargetArchDirErrorNamesDependency(t *testing.T) {
	_, err := linuxTargetArchDir("compiler-rt builtins", "windows-amd64")
	if err == nil {
		t.Fatal("expected an error for a non-Linux target")
	}
	msg := err.Error()
	for _, want := range []string{"compiler-rt builtins", "windows-amd64", "linux-amd64", "linux-arm64"} {
		if !contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
}

// TestTargetDepManifestNamesAreDistinctAcrossDeps pins the property the shared
// name former is relied on for: the same arch and the same `out` file name in
// two different dependencies must not collide in one host manifest. Nothing but
// the dep prefix separates them.
func TestTargetDepManifestNamesAreDistinctAcrossDeps(t *testing.T) {
	const arch, file = "aarch64-linux-musl", "libc.a"
	names := map[string]string{}
	for _, dep := range []string{"musl", "openssl", "compiler-rt"} {
		n := targetDepManifestName(dep, arch, file)
		if prev, ok := names[n]; ok {
			t.Errorf("%s and %s both produce manifest name %q", prev, dep, n)
		}
		names[n] = dep
	}
	// And the public formers must agree with the shared one — the compiler
	// side duplicates this format across a module boundary.
	if got, want := MuslManifestName(arch, "crt1.o"), targetDepManifestName("musl", arch, "crt1.o"); got != want {
		t.Errorf("MuslManifestName = %q, want %q", got, want)
	}
	if got, want := OpenSSLManifestName(arch, "libssl.a"), targetDepManifestName("openssl", arch, "libssl.a"); got != want {
		t.Errorf("OpenSSLManifestName = %q, want %q", got, want)
	}
	if got, want := CompilerRTManifestName(arch, "libclang_rt.builtins.a"), targetDepManifestName("compiler-rt", arch, "libclang_rt.builtins.a"); got != want {
		t.Errorf("CompilerRTManifestName = %q, want %q", got, want)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
