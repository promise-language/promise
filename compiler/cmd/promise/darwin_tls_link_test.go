package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"

	"github.com/promise-language/promise/compiler/internal/codegen/pal"
)

// TestBundledFrameworkTBDsCoverBackendSymbols is the guard for T1599's mandatory
// zero-dependency requirement: a TLS program must link on a macOS host with no
// Xcode Command Line Tools, using the hand-authored TBD stubs. That only holds if
// every Security/CoreFoundation symbol the Secure Transport backend declares is
// listed in a stub — a missing one fails the link only on machines without an SDK,
// which is exactly where nobody is looking.
//
// Rather than duplicate the symbol list, this derives it from the backend itself:
// emit the module and collect every bodyless framework declaration.
func TestBundledFrameworkTBDsCoverBackendSymbols(t *testing.T) {
	t.Parallel()
	module := ir.NewModule()
	p, ok := pal.ForTarget("arm64-apple-darwin").(*pal.PosixPAL)
	if !ok {
		t.Fatal("ForTarget(darwin) did not yield a PosixPAL")
	}
	p.EmitTLSSecureTransport(module)

	stubs := bundledSecurityTBD + bundledCoreFoundationTBD

	isFrameworkSym := func(name string) bool {
		return strings.HasPrefix(name, "SSL") || strings.HasPrefix(name, "Sec") ||
			strings.HasPrefix(name, "CF") || strings.HasPrefix(name, "kCF")
	}

	found := 0
	for _, fn := range module.Funcs {
		if len(fn.Blocks) != 0 || !isFrameworkSym(fn.Name()) {
			continue // definitions and non-framework externs (memcpy, pal_alloc, …)
		}
		found++
		if !strings.Contains(stubs, "_"+fn.Name()) {
			t.Errorf("Secure Transport backend declares %s but no bundled TBD stub "+
				"exports _%s — a no-Xcode build of a TLS program will fail to link",
				fn.Name(), fn.Name())
		}
	}
	for _, g := range module.Globals {
		if g.Init != nil || !isFrameworkSym(g.Name()) {
			continue
		}
		found++
		if !strings.Contains(stubs, "_"+g.Name()) {
			t.Errorf("backend references data symbol %s but no bundled TBD stub exports _%s",
				g.Name(), g.Name())
		}
	}
	if found == 0 {
		t.Fatal("no framework symbols discovered — the extraction is broken, not the stubs")
	}
}

// TestBundledFrameworkTBDsWellFormed pins the TBD shape ld64.lld needs: a v4
// document with the framework's real install-name, covering both Promise macOS
// arches. A wrong install-name links but produces a binary dyld cannot resolve.
func TestBundledFrameworkTBDsWellFormed(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, tbd, installName string }{
		{"Security", bundledSecurityTBD,
			"/System/Library/Frameworks/Security.framework/Versions/A/Security"},
		{"CoreFoundation", bundledCoreFoundationTBD,
			"/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"},
	} {
		if !strings.HasPrefix(tt.tbd, "--- !tapi-tbd\n") {
			t.Errorf("%s stub is not a tapi-tbd document", tt.name)
		}
		if !strings.Contains(tt.tbd, "tbd-version:     4") {
			t.Errorf("%s stub must be TBD v4", tt.name)
		}
		if !strings.Contains(tt.tbd, tt.installName) {
			t.Errorf("%s stub has the wrong install-name (want %s)", tt.name, tt.installName)
		}
		for _, arch := range []string{"x86_64-macos", "arm64-macos"} {
			if !strings.Contains(tt.tbd, arch) {
				t.Errorf("%s stub does not target %s", tt.name, arch)
			}
		}
	}
}

// TestBuildDarwinLinkArgsTLSFrameworks pins that the two frameworks are added iff
// the program actually uses TLS, so non-TLS binaries keep their existing link line.
func TestBuildDarwinLinkArgsTLSFrameworks(t *testing.T) {
	joined := func(needsTLS bool) string {
		return strings.Join(buildDarwinLinkArgs("arm64-apple-macosx14.0.0", "/tmp/x.o", "/tmp/x", needsTLS), " ")
	}

	with := joined(true)
	for _, want := range []string{"-framework Security", "-framework CoreFoundation"} {
		if !strings.Contains(with, want) {
			t.Errorf("needsTLS link args missing %q; got: %s", want, with)
		}
	}

	without := joined(false)
	for _, unwanted := range []string{"Security", "CoreFoundation", "-framework"} {
		if strings.Contains(without, unwanted) {
			t.Errorf("non-TLS link args must not mention %q; got: %s", unwanted, without)
		}
	}
	// Both forms still link libSystem and keep the object file.
	for _, want := range []string{"-lSystem", "/tmp/x.o"} {
		if !strings.Contains(without, want) || !strings.Contains(with, want) {
			t.Errorf("link args lost %q", want)
		}
	}
}

// TestEnsureBundledSDKWritesFrameworkTBDs pins where the framework stubs land.
// ld64 resolves `-framework Security` to exactly
// <sysroot>/System/Library/Frameworks/Security.framework/Security.tbd — the
// framework name is the file name, which is why no symlink is needed. A stub
// written anywhere else is simply never consulted, and the link then falls back to
// "undefined symbol" only on machines with no SDK.
func TestEnsureBundledSDKWritesFrameworkTBDs(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	sdk, err := ensureBundledSDK()
	if err != nil {
		t.Fatalf("ensureBundledSDK failed: %v", err)
	}

	for _, fw := range []struct{ name, want string }{
		{"Security", bundledSecurityTBD},
		{"CoreFoundation", bundledCoreFoundationTBD},
	} {
		path := filepath.Join(sdk.sysroot, "System", "Library", "Frameworks",
			fw.name+".framework", fw.name+".tbd")
		got, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s.tbd not written where -framework %s resolves: %v", fw.name, fw.name, err)
			continue
		}
		if string(got) != fw.want {
			t.Errorf("%s.tbd content does not match the bundled constant", fw.name)
		}
	}
}

// TestEnsureBundledSDKRewritesCorruptFrameworkTBD covers the repair branch: a stub
// left behind by an older compiler (or a truncated write) must be replaced, not
// trusted. Without this the cache would pin a stub that predates a newly added
// Secure Transport symbol and every no-Xcode TLS link would keep failing until the
// user wiped ~/.promise by hand.
func TestEnsureBundledSDKRewritesCorruptFrameworkTBD(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	fwDir := filepath.Join(tmp, "cache", "sdk", "macos", "System", "Library",
		"Frameworks", "Security.framework")
	if err := os.MkdirAll(fwDir, 0755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(fwDir, "Security.tbd")
	if err := os.WriteFile(stale, []byte("--- !tapi-tbd\nstale\n...\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := ensureBundledSDK(); err != nil {
		t.Fatalf("ensureBundledSDK failed: %v", err)
	}

	got, err := os.ReadFile(stale)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != bundledSecurityTBD {
		t.Error("a stale Security.tbd of a different size was not rewritten")
	}
}

// TestDarwinTLSLinksWithoutXcode is the direct verification of T1599's mandatory
// zero-dependency requirement, rather than an assumption about it: the Secure
// Transport backend is compiled to a real Mach-O object and linked against nothing
// but the hand-authored TBD stubs, with the bundled cache as the only sysroot. No
// Xcode Command Line Tools are consulted — ensureBundledSDK is called directly, so
// this exercises the no-CLT path even on a machine that has an SDK installed.
//
// It ends with a negative control. A link test that cannot fail proves nothing, and
// the ways this one could silently stop checking (the framework never consulted, the
// stub path wrong, undefined symbols tolerated) all look like a pass. So the same
// link is repeated against a Security stub with one symbol removed and is required
// to fail.
func TestDarwinTLSLinksWithoutXcode(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Mach-O link against the bundled macOS SDK stubs needs a darwin host")
	}
	llc, err := findLLVMTool("llc")
	if err != nil {
		t.Skipf("llc unavailable: %v", err)
	}
	linker, _, err := findDarwinLinker()
	if err != nil {
		t.Skipf("ld64.lld unavailable: %v", err)
	}

	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)
	sdk, err := ensureBundledSDK()
	if err != nil {
		t.Fatalf("ensureBundledSDK failed: %v", err)
	}

	tri := parseDarwinTriple(runtime.GOARCH + "-apple-macosx14.0.0")
	triple := tri.arch + "-apple-macosx" + tri.minVersion

	module := ir.NewModule()
	module.TargetTriple = triple
	p, ok := pal.ForTarget(triple).(*pal.PosixPAL)
	if !ok {
		t.Fatal("ForTarget(darwin) did not yield a PosixPAL")
	}
	p.EmitTLSSecureTransport(module)
	// pal_alloc/pal_free are Promise runtime symbols the real link gets from the
	// emitted PAL; stub them so the only unresolved names left are framework ones.
	for _, fn := range module.Funcs {
		if len(fn.Blocks) != 0 {
			continue
		}
		switch fn.Name() {
		case "pal_alloc":
			fn.NewBlock("").NewRet(constant.NewNull(irtypes.I8Ptr))
		case "pal_free":
			fn.NewBlock("").NewRet(nil)
		}
	}

	llPath := filepath.Join(tmp, "tls.ll")
	if err := os.WriteFile(llPath, []byte(module.String()), 0644); err != nil {
		t.Fatal(err)
	}
	objPath := filepath.Join(tmp, "tls.o")
	if out, err := exec.Command(llc, "-filetype=obj", "-mtriple="+triple, llPath, "-o", objPath).CombinedOutput(); err != nil {
		t.Fatalf("llc could not compile the Secure Transport backend: %v\n%s", err, out)
	}

	link := func(out string) ([]byte, error) {
		return exec.Command(linker,
			"-dylib", "-arch", tri.arch,
			"-platform_version", "macos", tri.minVersion, tri.minVersion,
			"-syslibroot", sdk.sysroot, "-o", out, objPath,
			"-lSystem", "-framework", "Security", "-framework", "CoreFoundation",
		).CombinedOutput()
	}

	if out, err := link(filepath.Join(tmp, "tls.dylib")); err != nil {
		t.Fatalf("a TLS program cannot link with no Xcode CLT — the bundled TBD stubs "+
			"are incomplete or misplaced (T1599): %v\n%s", err, out)
	}

	// Negative control — remove one symbol and require the link to break.
	secPath := filepath.Join(sdk.sysroot, "System", "Library", "Frameworks",
		"Security.framework", "Security.tbd")
	trimmed := strings.Replace(bundledSecurityTBD, "_SSLHandshake,", "", 1)
	if trimmed == bundledSecurityTBD {
		t.Fatal("negative control could not remove _SSLHandshake from the stub")
	}
	if err := os.WriteFile(secPath, []byte(trimmed), 0644); err != nil {
		t.Fatal(err)
	}
	if out, err := link(filepath.Join(tmp, "tls-neg.dylib")); err == nil {
		t.Fatalf("link succeeded with _SSLHandshake missing from the stub — this test "+
			"is not actually checking the stubs\n%s", out)
	} else if !strings.Contains(string(out), "_SSLHandshake") {
		t.Errorf("link failed for some reason other than the removed symbol:\n%s", out)
	}
}

// TestDarwinTLSIRVerifies runs the LLVM verifier over the Secure Transport
// backend's IR. The backend is ~1400 lines of hand-built IR — blocks wired by
// hand, GEPs indexed by hand — and today the only thing that structurally
// validates it is TestDarwinTLSLinksWithoutXcode, which skips off-darwin. So a
// malformed block, an unterminated one, or a GEP with the wrong index depth would
// go unnoticed on the Linux reference host right up until someone built on a Mac.
//
// Verifying darwin IR needs no darwin host — only the verifier — so this runs
// everywhere and skips only when the tool itself is missing.
func TestDarwinTLSIRVerifies(t *testing.T) {
	llvmAs, err := findLLVMTool("llvm-as")
	if err != nil {
		t.Skipf("llvm-as unavailable: %v", err)
	}

	for _, triple := range []string{"arm64-apple-macosx26.0.0", "x86_64-apple-macosx10.15.0"} {
		module := ir.NewModule()
		module.TargetTriple = triple
		p, ok := pal.ForTarget(triple).(*pal.PosixPAL)
		if !ok {
			t.Fatalf("%s did not resolve to a PosixPAL", triple)
		}
		p.EmitTLSSecureTransport(module)

		llPath := filepath.Join(t.TempDir(), "tls.ll")
		if err := os.WriteFile(llPath, []byte(module.String()), 0644); err != nil {
			t.Fatal(err)
		}
		// llvm-as parses and runs the module verifier; -o /dev/null keeps the
		// bitcode off disk since only the diagnostics matter.
		out, err := exec.Command(llvmAs, llPath, "-o", os.DevNull).CombinedOutput()
		if err != nil {
			t.Errorf("%s: Secure Transport IR does not verify:\n%s", triple, out)
		}
	}
}

// TestTLSCipherFallbackBufferFits pins a silent-corruption trap in
// pal_tls_get_cipher. An unrecognised suite is rendered as "0xXXXX" plus a NUL —
// seven bytes — into a fixed array embedded in the session record. That path never
// executes in the test suite, because every suite Secure Transport negotiates for
// the repo's certificate is in the name table, so a buffer shrunk to six bytes
// would write its terminator into the following struct member and nothing here
// would fail. The width is therefore checked structurally instead.
func TestTLSCipherFallbackBufferFits(t *testing.T) {
	t.Parallel()
	module := ir.NewModule()
	p, ok := pal.ForTarget("arm64-apple-macosx26.0.0").(*pal.PosixPAL)
	if !ok {
		t.Fatal("ForTarget(macosx) did not yield a PosixPAL")
	}
	p.EmitTLSSecureTransport(module)
	out := module.String()

	// The session struct's last member is the fallback buffer. "0xXXXX\0" needs 7.
	m := regexp.MustCompile(`\[(\d+) x i8\]\s*}`).FindStringSubmatch(out)
	if m == nil {
		t.Fatal("could not locate the session record's cipher fallback buffer")
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatal(err)
	}
	if n < 7 {
		t.Errorf("cipher fallback buffer is [%d x i8]; \"0xXXXX\" plus its NUL needs 7 — "+
			"a smaller one overflows into the next struct member on any unrecognised suite", n)
	}

	// The rendering must be lowercase-hex driven by a 16-entry digit table; a
	// truncated table would index out of bounds for nibbles above its length.
	if !strings.Contains(out, `c"0123456789abcdef\00"`) {
		t.Error("the hex digit table backing the fallback rendering is missing or truncated")
	}
}
