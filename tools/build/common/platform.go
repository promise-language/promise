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

const (
	LLVMMinVersion = 22
	LLVMMaxVersion = 25
)

// LLVMInfo holds the discovered LLVM tool paths and version.
type LLVMInfo struct {
	Version int    // e.g., 23
	OptPath string // path to opt
	LLCPath string // path to llc (may be empty on non-Windows)
	LLDPath string // path to lld/ld.lld/ld64.lld/lld-link
	Dir     string // LLVM base directory (for bundling)
}

// FindLLVM searches for LLVM tools on the current platform. The lookup order
// is: (1) explicit PROMISE_LLVM directory override, (2) system discovery
// (Homebrew/PATH/Program Files), (3) fetch the pinned LLVM from the slim
// blob catalog into the host-stable prebuilts cache (T0798). `root` is the
// repo root, used by the slim-fetch fallback to read prebuilts.toml +
// blobs.json. When the slim fetch fails (no network, no catalog entry, …) the
// error message lists every path tried so a developer can see why.
func FindLLVM(root string) (*LLVMInfo, error) {
	// 1. Explicit override — PROMISE_LLVM points at a directory holding the
	// per-prebuilts.toml `out` files (opt/llc/lld[.exe]). Wins outright so a
	// developer can run against an air-gapped toolchain or a corporate
	// prebuild without touching code.
	if dir := strings.TrimSpace(os.Getenv("PROMISE_LLVM")); dir != "" {
		if info, ok := llvmInfoFromDir(dir); ok {
			return info, nil
		}
		return nil, fmt.Errorf("PROMISE_LLVM=%q: no opt/llc/lld found under that directory", dir)
	}

	// 2. System discovery — Homebrew, /usr/lib/llvm-N, Program Files.
	info := &LLVMInfo{}
	switch runtime.GOOS {
	case "darwin":
		findLLVMDarwin(info)
	case "windows":
		findLLVMWindows(info)
	default: // linux and others
		findLLVMLinux(info)
	}
	if info.OptPath != "" {
		if info.LLDPath == "" {
			info.LLDPath = findLLD(info)
		}
		if info.LLDPath != "" {
			return info, nil
		}
	}

	// 3. Slim blob fallback — fetch pinned LLVM into the host-stable prebuilts
	// cache. This is what removes the "system LLVM required" setup step.
	if root != "" {
		if cacheDir, err := EnsureLLVMBlobs(root, CurrentBuildTarget()); err == nil {
			if info, ok := llvmInfoFromDir(cacheDir); ok {
				return info, nil
			}
		} else {
			return nil, fmt.Errorf("LLVM %d-%d not found in system search and slim-blob fetch failed: %w (set PROMISE_LLVM=<dir> to point at a local install)", LLVMMinVersion, LLVMMaxVersion, err)
		}
	}

	if info.OptPath == "" {
		return nil, fmt.Errorf("LLVM %d-%d not found (need opt in PATH, Homebrew, or set PROMISE_LLVM)", LLVMMinVersion, LLVMMaxVersion)
	}
	return nil, fmt.Errorf("lld not found (need lld in PATH or Homebrew, or set PROMISE_LLVM)")
}

// llvmInfoFromDir builds an LLVMInfo from a flat directory containing the
// per-prebuilts.toml `out` names (opt/llc/lld, plus the `.exe` variants on
// Windows). Used by both the PROMISE_LLVM override and the slim-fetch
// fallback. Returns (nil, false) when opt isn't present so the caller can
// surface a clear error.
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
	if info.LLDPath == "" {
		return nil, false
	}
	return info, true
}

func findLLVMDarwin(info *LLVMInfo) {
	brewPrefixes := []string{"/opt/homebrew/opt", "/usr/local/opt"}

	// Try versioned Homebrew installs (highest first)
	for v := LLVMMaxVersion; v >= LLVMMinVersion; v-- {
		for _, prefix := range brewPrefixes {
			dir := filepath.Join(prefix, fmt.Sprintf("llvm@%d", v))
			opt := filepath.Join(dir, "bin", "opt")
			if Exists(opt) {
				info.OptPath = opt
				info.Dir = dir
				info.Version = v
				info.LLCPath = filepath.Join(dir, "bin", "llc")
				// Check for lld in the same LLVM install
				lld := filepath.Join(dir, "bin", "ld64.lld")
				if Exists(lld) {
					info.LLDPath = lld
				}
				return
			}
		}
	}

	// Try unversioned Homebrew
	for _, prefix := range brewPrefixes {
		dir := filepath.Join(prefix, "llvm")
		opt := filepath.Join(dir, "bin", "opt")
		if Exists(opt) {
			ver := parseLLVMVersion(opt)
			if ver >= LLVMMinVersion && ver <= LLVMMaxVersion {
				info.OptPath = opt
				info.Dir = dir
				info.Version = ver
				info.LLCPath = filepath.Join(dir, "bin", "llc")
				lld := filepath.Join(dir, "bin", "ld64.lld")
				if Exists(lld) {
					info.LLDPath = lld
				}
				return
			}
		}
	}

	// Fallback: versioned in PATH
	findLLVMVersionedInPATH(info)
}

func findLLVMLinux(info *LLVMInfo) {
	// Versioned in PATH (highest first)
	for v := LLVMMaxVersion; v >= LLVMMinVersion; v-- {
		name := fmt.Sprintf("opt-%d", v)
		if path := Which(name); path != "" {
			info.OptPath = path
			info.Version = v
			info.LLCPath = Which(fmt.Sprintf("llc-%d", v))
			info.Dir = fmt.Sprintf("/usr/lib/llvm-%d", v)
			lld := Which(fmt.Sprintf("ld.lld-%d", v))
			if lld != "" {
				info.LLDPath = lld
			}
			return
		}
	}

	// Unversioned
	if path := Which("opt"); path != "" {
		ver := parseLLVMVersion(path)
		if ver >= LLVMMinVersion && ver <= LLVMMaxVersion {
			info.OptPath = path
			info.Version = ver
			info.LLCPath = Which("llc")
			return
		}
	}
}

func findLLVMWindows(info *LLVMInfo) {
	searchDirs := []string{
		filepath.Join(os.Getenv("ProgramFiles"), "LLVM", "bin"),
		filepath.Join(os.Getenv("USERPROFILE"), "LLVM", "bin"),
	}
	for _, dir := range searchDirs {
		opt := filepath.Join(dir, "opt.exe")
		if Exists(opt) {
			ver := parseLLVMVersion(opt)
			if ver >= LLVMMinVersion && ver <= LLVMMaxVersion {
				info.OptPath = opt
				info.Dir = filepath.Dir(dir) // parent of bin/
				info.Version = ver
				info.LLCPath = filepath.Join(dir, "llc.exe")
				lld := filepath.Join(dir, "lld-link.exe")
				if Exists(lld) {
					info.LLDPath = lld
				}
				return
			}
		}
	}

	// Fallback: PATH
	if path := Which("opt"); path != "" {
		ver := parseLLVMVersion(path)
		if ver >= LLVMMinVersion && ver <= LLVMMaxVersion {
			info.OptPath = path
			info.Version = ver
			info.LLCPath = Which("llc")
			return
		}
	}
}

func findLLVMVersionedInPATH(info *LLVMInfo) {
	for v := LLVMMaxVersion; v >= LLVMMinVersion; v-- {
		name := fmt.Sprintf("opt-%d", v)
		if path := Which(name); path != "" {
			info.OptPath = path
			info.Version = v
			info.LLCPath = Which(fmt.Sprintf("llc-%d", v))
			return
		}
	}

	// Unversioned fallback
	if path := Which("opt"); path != "" {
		ver := parseLLVMVersion(path)
		if ver >= LLVMMinVersion && ver <= LLVMMaxVersion {
			info.OptPath = path
			info.Version = ver
			info.LLCPath = Which("llc")
			return
		}
	}
}

func findLLD(info *LLVMInfo) string {
	switch runtime.GOOS {
	case "darwin":
		return findLLDDarwin(info)
	case "windows":
		return findLLDWindows(info)
	default:
		return findLLDLinux(info)
	}
}

func findLLDDarwin(info *LLVMInfo) string {
	brewPrefixes := []string{"/opt/homebrew/opt", "/usr/local/opt"}

	// Check if lld is in the LLVM directory we already found
	if info.Dir != "" {
		lld := filepath.Join(info.Dir, "bin", "ld64.lld")
		if Exists(lld) {
			return lld
		}
	}

	// Separate lld package
	for _, prefix := range brewPrefixes {
		lld := filepath.Join(prefix, "lld", "bin", "ld64.lld")
		if Exists(lld) {
			return lld
		}
		// Also check under llvm
		lld = filepath.Join(prefix, "llvm", "bin", "ld64.lld")
		if Exists(lld) {
			return lld
		}
	}

	// Versioned in PATH
	for v := LLVMMaxVersion; v >= LLVMMinVersion; v-- {
		for _, name := range []string{
			fmt.Sprintf("ld64.lld-%d", v),
			fmt.Sprintf("lld-%d", v),
		} {
			if path := Which(name); path != "" {
				return path
			}
		}
	}

	// Unversioned
	if path := Which("ld64.lld"); path != "" {
		return path
	}
	return Which("lld")
}

func findLLDLinux(info *LLVMInfo) string {
	// Versioned
	for v := LLVMMaxVersion; v >= LLVMMinVersion; v-- {
		for _, name := range []string{
			fmt.Sprintf("ld.lld-%d", v),
			fmt.Sprintf("lld-%d", v),
		} {
			if path := Which(name); path != "" {
				return path
			}
		}
	}
	// Unversioned
	if path := Which("ld.lld"); path != "" {
		return path
	}
	return Which("lld")
}

func findLLDWindows(info *LLVMInfo) string {
	if info.Dir != "" {
		lld := filepath.Join(info.Dir, "bin", "lld-link.exe")
		if Exists(lld) {
			return lld
		}
	}
	if path := Which("lld-link"); path != "" {
		return path
	}
	return Which("lld")
}

var versionRe = regexp.MustCompile(`version (\d+)\.`)

// parseLLVMVersion runs "opt --version" and extracts the major version number.
func parseLLVMVersion(optPath string) int {
	out, err := RunOutputQuiet(optPath, "--version")
	if err != nil {
		return 0
	}
	m := versionRe.FindStringSubmatch(out)
	if m == nil {
		return 0
	}
	v, _ := strconv.Atoi(m[1])
	return v
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
