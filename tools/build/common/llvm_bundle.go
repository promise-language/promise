package common

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// BundleLLVM gzips LLVM tools into the resources directory for release builds.
func BundleLLVM(root string, llvm *LLVMInfo) error {
	if IsDarwin() {
		return bundleLLVMDarwin(root, llvm)
	}
	if IsLinux() {
		return bundleLLVMLinux(root, llvm)
	}
	fmt.Println("WARNING: LLVM bundling not supported on " + GoOS())
	return nil
}

func bundleLLVMLinux(root string, llvm *LLVMInfo) error {
	dst := filepath.Join(root, "compiler", "cmd", "promise", "resources", "llvm", "linux-amd64")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}

	llvmDir := llvm.Dir
	if llvmDir == "" {
		llvmDir = fmt.Sprintf("/usr/lib/llvm-%d", llvm.Version)
	}

	fmt.Printf("Bundling LLVM %d tools (%s)...\n", llvm.Version, llvmDir)

	// Find the real libLLVM.so (resolve symlink)
	libLLVM := filepath.Join(llvmDir, "lib", "libLLVM.so")
	realLib, err := filepath.EvalSymlinks(libLLVM)
	if err != nil {
		return fmt.Errorf("resolve libLLVM.so: %w", err)
	}

	files := map[string]string{
		filepath.Join(llvmDir, "bin", "opt"): "opt.gz",
		filepath.Join(llvmDir, "bin", "llc"): "llc.gz",
		filepath.Join(llvmDir, "bin", "lld"): "lld.gz",
		realLib:                              "libLLVM.so.gz",
	}

	for src, name := range files {
		if err := gzipFile(src, filepath.Join(dst, name)); err != nil {
			return fmt.Errorf("gzip %s: %w", name, err)
		}
		printSize(filepath.Join(dst, name))
	}
	return nil
}

func bundleLLVMDarwin(root string, llvm *LLVMInfo) error {
	dst := filepath.Join(root, "compiler", "cmd", "promise", "resources", "llvm", "darwin-"+GoArch())
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}

	brewLLVM := llvm.Dir
	if brewLLVM == "" {
		return fmt.Errorf("LLVM directory not found for bundling")
	}

	// Find lld directory (may be separate from LLVM)
	brewLLDDir := brewLLVM
	lldBin := filepath.Join(brewLLVM, "bin", "lld")
	if !Exists(lldBin) {
		// Check separate lld package
		for _, prefix := range []string{"/opt/homebrew/opt/lld", "/usr/local/opt/lld"} {
			if Exists(filepath.Join(prefix, "bin", "lld")) {
				brewLLDDir = prefix
				break
			}
		}
	}

	fmt.Printf("Bundling LLVM %d tools from %s + lld from %s (darwin-%s)...\n",
		llvm.Version, brewLLVM, brewLLDDir, GoArch())

	// Core tools
	files := map[string]string{
		filepath.Join(brewLLVM, "bin", "opt"):           "opt.gz",
		filepath.Join(brewLLVM, "bin", "llc"):           "llc.gz",
		filepath.Join(brewLLDDir, "bin", "lld"):         "lld.gz",
		filepath.Join(brewLLVM, "lib", "libLLVM.dylib"): "libLLVM.dylib.gz",
	}

	for src, name := range files {
		if err := gzipFile(src, filepath.Join(dst, name)); err != nil {
			return fmt.Errorf("gzip %s: %w", name, err)
		}
		printSize(filepath.Join(dst, name))
	}

	// lld dylibs
	lldLibDir := filepath.Join(brewLLDDir, "lib")
	if Exists(lldLibDir) {
		entries, _ := os.ReadDir(lldLibDir)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "liblld") && strings.HasSuffix(e.Name(), ".dylib") {
				src := filepath.Join(lldLibDir, e.Name())
				name := e.Name() + ".gz"
				if err := gzipFile(src, filepath.Join(dst, name)); err != nil {
					return fmt.Errorf("gzip %s: %w", name, err)
				}
				printSize(filepath.Join(dst, name))
			}
		}
	}

	// Dynamic dependencies of libLLVM.dylib (from Homebrew)
	deps, err := otoolDeps(filepath.Join(brewLLVM, "lib", "libLLVM.dylib"))
	if err == nil {
		for _, dep := range deps {
			name := filepath.Base(dep) + ".gz"
			if err := gzipFile(dep, filepath.Join(dst, name)); err != nil {
				fmt.Fprintf(os.Stderr, "  warning: could not bundle %s: %v\n", dep, err)
				continue
			}
			printSize(filepath.Join(dst, name))
		}
	}

	return nil
}

// otoolDeps returns Homebrew dependency paths from otool -L output,
// excluding libLLVM itself.
func otoolDeps(dylib string) ([]string, error) {
	out, err := RunOutput("otool", "-L", dylib)
	if err != nil {
		return nil, err
	}
	var deps []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		parts := strings.Fields(line)
		if len(parts) == 0 {
			continue
		}
		path := parts[0]
		if (strings.HasPrefix(path, "/opt/homebrew/") || strings.HasPrefix(path, "/usr/local/")) &&
			!strings.Contains(path, "libLLVM") {
			deps = append(deps, path)
		}
	}
	return deps, nil
}

func gzipFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	gz, err := gzip.NewWriterLevel(out, gzip.BestCompression)
	if err != nil {
		return err
	}
	if _, err := io.Copy(gz, in); err != nil {
		gz.Close()
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return out.Close()
}

func printSize(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	name := filepath.Base(path)
	size := float64(info.Size()) / (1024 * 1024)
	fmt.Printf("  %s: %.1f MB\n", name, size)
}
