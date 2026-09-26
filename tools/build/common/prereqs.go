package common

import (
	"fmt"
	"runtime"
	"strings"
)

// prereqsUsage is RunPrereqs's command line.
const prereqsUsage = "usage: bin/prereqs [-wasm]"

// RunPrereqs checks and reports on build prerequisites.
// On macOS/Linux it provides install guidance for the things a host genuinely
// has to supply (Go, Java); everything pinned is reported, not prescribed.
//
// `-wasm` stages the pinned WASM test runtimes instead of only reporting them.
// It used to be an install instruction printed in two error messages and
// implemented nowhere — `bin/prereqs -wasm` was advertised by the wasm gate and
// silently did nothing, because this function discarded its arguments. Since
// T2169 the runtimes are pinned, so the useful action is a materialization,
// exactly like the LLVM view.
func RunPrereqs(root string, args []string) error {
	stageWasm := false
	for _, arg := range NormalizeArgs(args) {
		switch arg {
		case "-wasm":
			stageWasm = true
		default:
			return fmt.Errorf("%s", prereqsUsage)
		}
	}

	fmt.Println("=== Promise Compiler Prerequisites ===")
	fmt.Printf("Platform: %s/%s\n\n", runtime.GOOS, runtime.GOARCH)

	ok := true

	// Go
	if path := Which("go"); path != "" {
		ver, _ := RunOutputQuiet("go", "version")
		fmt.Printf("✅ go:       %s\n", ver)
	} else {
		fmt.Println("❌ go:       NOT FOUND — install Go 1.25+ from https://go.dev/dl/")
		ok = false
	}

	// Java (for ANTLR) — java -version writes to stderr
	if path := Which("java"); path != "" { // path-ok: bin/prereqs reports host state — that is its whole subject
		ver, _ := RunOutputCombined("java", "-version")
		fmt.Printf("✅ java:     %s\n", firstLine(ver))
	} else {
		fmt.Println("❌ java:     NOT FOUND — install Java 11+ (for ANTLR parser generation)")
		ok = false
	}

	// LLVM — the pinned toolchain, fetched on demand. Never a system install:
	// no package manager is suggested here, because installing one would not
	// affect the build (T2108).
	llvm, err := FindLLVM(root)
	if err != nil {
		fmt.Printf("❌ llvm:     NOT AVAILABLE — %v\n", err)
		ok = false
	} else {
		fmt.Printf("✅ llvm:     %d (opt: %s)\n", llvm.Version, llvm.OptPath)
		fmt.Printf("✅ lld:      %s\n", llvm.LLDPath)
	}

	// musl CRT (Linux only) — a fetched prebuilt since T0530, never a system
	// package. Reported for visibility only: `bin/build` fetches it on demand,
	// so a cold cache is normal and must not fail the prereq check.
	if IsLinux() {
		if arch, err := MuslArchDir(CurrentBuildTarget()); err != nil {
			fmt.Printf("❌ musl:     no CRT prebuilt for %s\n", CurrentBuildTarget())
			ok = false
		} else {
			fmt.Printf("✅ musl:     %s CRT fetched by bin/build (no system musl-dev needed)\n", arch)
		}
	}

	// WASM test runtimes — pinned prebuilts since T2169, never system installs.
	// Like LLVM above, no package manager is suggested: installing one would not
	// affect what a wasm run executes.
	for _, dep := range WasmRuntimeDeps() {
		reportWasmRuntime(root, dep, stageWasm)
	}

	fmt.Println()
	if ok {
		fmt.Println("All required prerequisites installed.")
	} else {
		fmt.Println("Some required prerequisites are missing — see install instructions above.")
	}
	return nil
}

// reportWasmRuntime reports one pinned WASM test runtime, staging it first when
// `stage` is set (`bin/prereqs -wasm`).
//
// Without -wasm a runtime that is not staged yet is NOT a failure: it is fetched
// on demand by the wasm gates and by `promise test --target wasm32-*`, so a cold
// cache is the normal state of a machine that has not run wasm tests — the same
// reason the musl CRT above is reported for visibility only.
//
// A host copy is reported when one exists, explicitly marked as unused. That
// line is the whole remaining value of a PATH probe here: it is what answers
// "I have wasmtime 41 installed, why is the suite running 44".
func reportWasmRuntime(root, dep string, stage bool) {
	// Same column as the rows above ("go:", "llvm:", "musl:"), which are padded
	// by hand in the literals there; a width here keeps this pair aligned with
	// them without a second spelling of the number per line.
	label := fmt.Sprintf("%-9s", dep+":")

	version, target := "unknown", CurrentBuildTarget()
	if pm, err := LoadPrebuiltsManifest(root); err == nil && pm.Binaries[dep] != nil {
		version = pm.Binaries[dep].Version
		if t := pm.Binaries[dep].Targets[target]; t == nil || t.Unsupported != "" {
			fmt.Printf("❌ %s no pinned %s for %s\n", label, dep, target)
			return
		}
	}

	if stage {
		path, err := EnsureWasmRuntime(root, dep)
		if err != nil {
			fmt.Printf("❌ %s %s %s could not be staged: %v\n", label, dep, version, err)
			return
		}
		fmt.Printf("✅ %s %s %s (pinned, staged at %s)\n", label, dep, version, path)
	} else {
		fmt.Printf("✅ %s %s %s (pinned; fetched on demand — stage now with `bin/prereqs -wasm`)\n", label, dep, version)
	}

	// Naming an unused host copy is precisely what makes this report useful.
	if hostCopy := Which(dep); hostCopy != "" { // path-ok: bin/prereqs reports host state — that is its whole subject
		fmt.Printf("            note: %s is also installed at %s — NOT used (the pinned %s runs)\n", dep, hostCopy, version)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
