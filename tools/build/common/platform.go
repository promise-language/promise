package common

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// LLVMMinVersion is the minimum LLVM major the generated IR requires
// (llvm.coro.end returns void from 22 on). It is not a search range: the
// toolchain is whatever the pinned blobs hold, not whatever a host happens to
// have (T2108).
const LLVMMinVersion = 22

// LLVMInfo holds the resolved LLVM tool paths and version.
type LLVMInfo struct {
	Version int    // e.g., 23
	OptPath string // path to opt
	LLCPath string // path to llc (may be empty on non-Windows)
	LLDPath string // path to lld/ld.lld/ld64.lld/lld-link
	// DlltoolPath is the path to llvm-dlltool when resolved from the pinned slim
	// cache (the prebuilts.toml build-only entry, T0833). Empty when not present
	// — it is optional (only the winlink import-lib generator needs it) and never
	// required for the (info, true) resolution contract.
	DlltoolPath string
	Dir         string // LLVM base directory (for bundling)
}

// toolchainOverrideVars lists the environment variables that point at a single
// LLVM binary each. Setting one is the only way a toolchain from outside the
// pinned set enters a build, and it is the supported path for bringing Promise
// up on a new LLVM version (T2108).
//
// This is the same vocabulary the compiler uses (llvmToolEnvVars in
// compiler/cmd/promise/main.go). The compiler and the build tools are separate
// Go modules, so the small, stable list is spelled once per module rather than
// imported; keep the two in step.
var toolchainOverrideVars = []string{
	"PROMISE_OPT",
	"PROMISE_LLC",
	"PROMISE_LLD",
	"PROMISE_LD64LLD",
	"PROMISE_WASM_LD",
	"PROMISE_CLANG",
	"PROMISE_USE_CLANG",
}

// ToolchainOverrides returns every toolchain override in effect as
// "NAME=value", in declaration order, or nil when the build is on the pinned
// toolchain. Callers that measure the tree (gates) refuse to report under one;
// callers that build announce it and continue.
func ToolchainOverrides() []string {
	var out []string
	for _, name := range toolchainOverrideVars {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			out = append(out, name+"="+v)
		}
	}
	return out
}

// announceToolchainOverrides prints every override in effect. Unconditional and
// not gated on a verbosity flag: an override left in a shell profile would
// otherwise silently build against a different toolchain while the operator
// believes they are on the pinned one (T2108).
func announceToolchainOverrides() {
	overrides := ToolchainOverrides()
	if len(overrides) == 0 {
		return
	}
	// Same shape as the compiler's banner (announceToolchainOverride in
	// compiler/cmd/promise/main.go): the consequence heads it, the variables
	// that caused it are listed under it.
	fmt.Fprintln(os.Stderr, "warning: LLVM toolchain override in effect — this build does NOT use the pinned toolchain:")
	for _, o := range overrides {
		fmt.Fprintf(os.Stderr, "warning:   %s\n", o)
	}
}

// linkerOverrideVar returns the override variable naming this platform's linker
// binary, so a per-tool override substitutes exactly one tool.
func linkerOverrideVar() string {
	if IsDarwin() {
		return "PROMISE_LD64LLD"
	}
	return "PROMISE_LLD"
}

// FindLLVM resolves the LLVM tools from the two sources a build may take them
// from (T2108):
//
//  1. The pinned LLVM blobs, fetched into the host-stable prebuilts cache
//     (T0798). `root` is the repo root, used to read prebuilts.toml + blobs.json.
//  2. Per-tool environment overrides (PROMISE_OPT, PROMISE_LLC, and the
//     platform linker's PROMISE_LLD / PROMISE_LD64LLD), overlaid on top — the
//     explicit new-LLVM-bringup path, announced whenever it is in effect.
//
// PATH, Homebrew and Program Files are never consulted. A toolchain discovered
// from incidental host state makes the build depend on what the machine happens
// to have installed; absent a pinned toolchain the build fails and says so.
func FindLLVM(root string) (*LLVMInfo, error) {
	announceToolchainOverrides()

	suffix := ExeSuffix()
	optOverride := strings.TrimSpace(os.Getenv("PROMISE_OPT"))
	llcOverride := strings.TrimSpace(os.Getenv("PROMISE_LLC"))
	lldOverride := strings.TrimSpace(os.Getenv(linkerOverrideVar()))

	// Fully overridden: the operator named every binary the build needs, so the
	// pinned set is not consulted at all (the bringup case, where the pinned
	// blobs may not exist for the LLVM being brought up).
	if optOverride != "" && lldOverride != "" {
		info := &LLVMInfo{
			OptPath: optOverride,
			LLCPath: llcOverride,
			LLDPath: lldOverride,
			Dir:     filepath.Dir(optOverride),
			Version: parseLLVMVersion(optOverride),
		}
		return info, nil
	}

	if root == "" {
		return nil, fmt.Errorf("no pinned LLVM toolchain: no repo root to read tools/build/prebuilts.toml from\n"+
			"  PATH and Homebrew are never consulted — build inputs are pinned, not discovered\n"+
			"  bringing Promise up on a new LLVM version? point at it explicitly: PROMISE_OPT=… PROMISE_LLC=… %s=…",
			linkerOverrideVar())
	}

	cacheDir, err := EnsureLLVMBlobs(root, CurrentBuildTarget())
	if err != nil {
		return nil, fmt.Errorf("no pinned LLVM toolchain for %s: %w\n"+
			"  PATH and Homebrew are never consulted — build inputs are pinned, not discovered\n"+
			"  bringing Promise up on a new LLVM version? point at it explicitly: PROMISE_OPT=… PROMISE_LLC=… %s=…",
			CurrentBuildTarget(), err, linkerOverrideVar())
	}

	info, ok := llvmInfoFromDir(cacheDir)
	if !ok {
		missing := filepath.Join(cacheDir, "opt"+suffix)
		if Exists(missing) {
			missing = filepath.Join(cacheDir, "lld"+suffix)
		}
		return nil, fmt.Errorf("pinned toolchain staged into %q but a required file is missing: %s (the prebuilts.toml files list for this target may be incomplete)", cacheDir, missing)
	}

	// A fetched toolchain is not automatically a usable one: the upstream Linux
	// tarballs are dynamically linked against glibc, so on a musl host (Alpine)
	// every tool fails to exec. Probe it here — otherwise the build "succeeds"
	// and ships a compiler that dies at the first `opt` invocation, far from the
	// cause.
	if _, verr := probeLLVMVersion(info.OptPath); verr != nil {
		return nil, fmt.Errorf("pinned LLVM prebuilt for %s cannot run on this host: %w\n"+
			"  probed: %s --version\n"+
			"  the upstream LLVM release binaries are dynamically linked against glibc "+
			"(ld-linux, libc.so.6, libstdc++.so.6); a musl host such as Alpine cannot execute them\n"+
			"  fix: build on a glibc host, or point at an LLVM %d+ built for this host: PROMISE_OPT=… PROMISE_LLC=… %s=…",
			CurrentBuildTarget(), verr, info.OptPath, LLVMMinVersion, linkerOverrideVar())
	}

	// Per-tool overlay: an override substitutes exactly that one binary, so the
	// variable itself states which tool is not the pinned one.
	if optOverride != "" {
		info.OptPath = optOverride
		info.Version = parseLLVMVersion(optOverride)
	}
	if llcOverride != "" {
		info.LLCPath = llcOverride
	}
	if lldOverride != "" {
		info.LLDPath = lldOverride
	}
	return info, nil
}

// llvmInfoFromDir builds an LLVMInfo from a flat directory containing the
// per-prebuilts.toml `out` names (opt/llc/lld, plus the `.exe` variants on
// Windows, and the optional build-only llvm-dlltool). Returns (nil, false) when
// opt or lld isn't present so the caller can surface a clear error.
//
// llvm-dlltool is optional: it only ships as a build-only slim-cache entry on a
// prebuilt-only host (T0833) and is unrelated to the opt+lld resolution
// contract, so a missing dlltool leaves DlltoolPath empty without failing.
func llvmInfoFromDir(dir string) (*LLVMInfo, bool) {
	suffix := ExeSuffix()
	opt := filepath.Join(dir, "opt"+suffix)
	if !Exists(opt) {
		return nil, false
	}
	info := &LLVMInfo{
		OptPath: opt,
		Dir:     dir,
		Version: parseLLVMVersion(opt),
	}
	if llc := filepath.Join(dir, "llc"+suffix); Exists(llc) {
		info.LLCPath = llc
	}
	if lld := filepath.Join(dir, "lld"+suffix); Exists(lld) {
		info.LLDPath = lld
	}
	if dt := filepath.Join(dir, "llvm-dlltool"+suffix); Exists(dt) {
		info.DlltoolPath = dt
	}
	if info.LLDPath == "" {
		return nil, false
	}
	return info, true
}

var versionRe = regexp.MustCompile(`version (\d+)\.`)

// parseLLVMVersion runs "opt --version" and extracts the major version number.
// Returns 0 when the tool cannot run or its output is unparseable — callers that
// need to tell those two cases apart (or report WHY) use probeLLVMVersion.
func parseLLVMVersion(optPath string) int {
	v, err := probeLLVMVersion(optPath)
	if err != nil {
		return 0
	}
	return v
}

// probeLLVMVersion runs "<opt> --version" and returns the major version, or an
// error describing why the tool is unusable. The distinction matters for fetched
// prebuilts: "0" from parseLLVMVersion silently reads as "unknown version", but
// an exec failure means the binary cannot run on this host at all (wrong libc)
// and must stop the build with an explanation.
func probeLLVMVersion(optPath string) (int, error) {
	out, err := RunOutputQuiet(optPath, "--version")
	if err != nil {
		return 0, err
	}
	m := versionRe.FindStringSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("unparseable `--version` output: %q", strings.TrimSpace(out))
	}
	v, _ := strconv.Atoi(m[1])
	return v, nil
}

// IsLinux returns true on Linux.
func IsLinux() bool { return runtime.GOOS == "linux" }

// IsDarwin returns true on macOS.
func IsDarwin() bool { return runtime.GOOS == "darwin" }

// IsWindows returns true on Windows.
func IsWindows() bool { return runtime.GOOS == "windows" }

// GoArch returns the Go architecture (e.g., "arm64", "amd64").
func GoArch() string { return runtime.GOARCH }

// GoOS returns the Go OS name.
func GoOS() string { return runtime.GOOS }

// ExeSuffix returns ".exe" on Windows, empty string otherwise.
func ExeSuffix() string {
	if IsWindows() {
		return ".exe"
	}
	return ""
}

// BinaryName returns "promise" with platform-appropriate suffix.
func BinaryName() string {
	return "promise" + ExeSuffix()
}

// PrintPlatform prints the current platform for diagnostics.
func PrintPlatform() string {
	return strings.ToUpper(runtime.GOOS[:1]) + runtime.GOOS[1:] + "/" + runtime.GOARCH
}
