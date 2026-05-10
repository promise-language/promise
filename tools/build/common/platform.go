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

// FindLLVM searches for LLVM tools on the current platform.
func FindLLVM() (*LLVMInfo, error) {
	info := &LLVMInfo{}

	// Find opt (and determine version + directory)
	switch runtime.GOOS {
	case "darwin":
		findLLVMDarwin(info)
	case "windows":
		findLLVMWindows(info)
	default: // linux and others
		findLLVMLinux(info)
	}

	if info.OptPath == "" {
		return nil, fmt.Errorf("LLVM %d-%d not found (need opt in PATH or Homebrew)", LLVMMinVersion, LLVMMaxVersion)
	}

	// Find lld
	if info.LLDPath == "" {
		info.LLDPath = findLLD(info)
	}
	if info.LLDPath == "" {
		return nil, fmt.Errorf("lld not found (need lld in PATH or Homebrew)")
	}

	return info, nil
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
