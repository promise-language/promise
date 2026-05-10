package common

import (
	"fmt"
	"runtime"
	"strings"
)

// RunPrereqs checks and reports on build prerequisites.
// On macOS/Linux it provides install guidance; actual installation
// still uses the platform-specific scripts for privileged operations.
func RunPrereqs(root string, args []string) error {
	fmt.Println("=== Promise Compiler Prerequisites ===")
	fmt.Printf("Platform: %s/%s\n\n", runtime.GOOS, runtime.GOARCH)

	ok := true

	// Go
	if path := Which("go"); path != "" {
		ver, _ := RunOutputQuiet("go", "version")
		fmt.Printf("  go:       %s\n", ver)
	} else {
		fmt.Println("  go:       NOT FOUND — install Go 1.25+ from https://go.dev/dl/")
		ok = false
	}

	// Java (for ANTLR) — java -version writes to stderr
	if path := Which("java"); path != "" {
		ver, _ := RunOutputCombined("java", "-version")
		fmt.Printf("  java:     %s\n", firstLine(ver))
	} else {
		fmt.Println("  java:     NOT FOUND — install Java 11+ (for ANTLR parser generation)")
		ok = false
	}

	// LLVM
	llvm, err := FindLLVM()
	if err != nil {
		fmt.Printf("  llvm:     NOT FOUND — %v\n", err)
		ok = false
		switch runtime.GOOS {
		case "darwin":
			fmt.Println("            Install: brew install llvm")
		case "linux":
			fmt.Println("            Install: sudo apt-get install llvm-22 lld-22")
		case "windows":
			fmt.Println("            Install: download from https://github.com/llvm/llvm-project/releases")
		}
	} else {
		fmt.Printf("  llvm:     %d (opt: %s)\n", llvm.Version, llvm.OptPath)
		fmt.Printf("  lld:      %s\n", llvm.LLDPath)
	}

	// musl (Linux only)
	if IsLinux() {
		if Exists("/usr/lib/x86_64-linux-musl/libc.a") {
			fmt.Println("  musl:     installed")
		} else {
			fmt.Println("  musl:     NOT FOUND — install: sudo apt-get install musl-dev")
			ok = false
		}
	}

	// wasmtime (optional)
	if path := Which("wasmtime"); path != "" {
		ver, _ := RunOutputQuiet("wasmtime", "--version")
		fmt.Printf("  wasmtime: %s\n", ver)
	} else {
		fmt.Println("  wasmtime: NOT FOUND (optional, for --target wasm32-wasi)")
	}

	fmt.Println()
	if ok {
		fmt.Println("All prerequisites installed.")
	} else {
		fmt.Println("Some prerequisites are missing. See above for install instructions.")
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
