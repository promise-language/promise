package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/promise-language/promise/compiler/internal/blobstore"
	"github.com/promise-language/promise/compiler/internal/module"
)

type doctorStatus int

const (
	doctorOK   doctorStatus = iota // [✓]
	doctorWarn                     // [!]
	doctorErr                      // [✗]
)

func (s doctorStatus) String() string {
	switch s {
	case doctorOK:
		return "ok"
	case doctorWarn:
		return "warning"
	default:
		return "error"
	}
}

type doctorCheck struct {
	Name     string   `json:"name"`
	Status   string   `json:"status"`
	Required bool     `json:"required"`
	Summary  string   `json:"summary"`
	Details  []string `json:"details,omitempty"`
	Fix      string   `json:"fix,omitempty"`
}

type doctorReport struct {
	Checks   []doctorCheck `json:"checks"`
	Errors   int           `json:"errors"`
	Warnings int           `json:"warnings"`
}

type doctorFlags struct {
	jsonOutput bool
	fix        bool
	network    bool
	dev        bool
	repair     bool
}

func runDoctor(args []string) {
	var flags doctorFlags
	for _, arg := range args {
		switch arg {
		case "-json":
			flags.jsonOutput = true
		case "-fix":
			flags.fix = true
		case "-network":
			flags.network = true
		case "-dev":
			flags.dev = true
		case "-repair", "--repair":
			flags.repair = true
		}
	}

	checks := []doctorCheck{
		doctorCheckInstallation(),
		doctorCheckLLVM(),
	}
	if runtime.GOOS == "linux" {
		checks = append(checks, doctorCheckMuslCRT())
	}
	checks = append(checks,
		doctorCheckBuildCache(),
		doctorCheckModuleCache(flags.network),
		doctorCheckPromiseHome(),
		doctorCheckEpochs(),
		doctorCheckCAS(flags),
	)
	if runtime.GOOS == "darwin" {
		checks = append(checks, doctorCheckXcodeCLT())
	}
	// Dev-only checks (compiler development / non-native targets) are gated behind
	// -dev. They are never required to build/run/test native Promise programs from a
	// release binary, so a fresh end-user install should not warn about them (T0819).
	if flags.dev {
		checks = append(checks,
			doctorCheckJava(),
			doctorCheckWasmtime(),
			doctorCheckNode(),
		)
	}
	checks = append(checks, doctorCheckPath())

	var report doctorReport
	report.Checks = checks
	var requiredErrors int
	for _, c := range checks {
		switch c.Status {
		case "error":
			report.Errors++
			if c.Required {
				requiredErrors++
			}
		case "warning":
			report.Warnings++
		}
	}

	if flags.jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(report)
	} else {
		printDoctorReport(report, flags)
	}

	if requiredErrors > 0 {
		os.Exit(1)
	}
}

func printDoctorReport(report doctorReport, flags doctorFlags) {
	fmt.Println("Promise doctor — checking your environment")
	fmt.Println()

	for _, c := range report.Checks {
		var icon string
		switch c.Status {
		case "ok":
			icon = "[✓]"
		case "warning":
			icon = "[!]"
		default:
			icon = "[✗]"
		}
		fmt.Printf("%s %s\n", icon, c.Name)
		if c.Summary != "" {
			fmt.Printf("    %s\n", c.Summary)
		}
		for _, d := range c.Details {
			fmt.Printf("    %s\n", d)
		}
		if flags.fix && c.Fix != "" {
			fmt.Printf("    Fix: %s\n", c.Fix)
		}
		fmt.Println()
	}

	// Summary line
	if report.Errors == 0 && report.Warnings == 0 {
		fmt.Println("No issues found.")
	} else {
		var parts []string
		if report.Errors > 0 {
			parts = append(parts, fmt.Sprintf("%d error(s)", report.Errors))
		}
		if report.Warnings > 0 {
			parts = append(parts, fmt.Sprintf("%d warning(s)", report.Warnings))
		}
		fmt.Println(strings.Join(parts, ", "))
	}
}

func makeDoctorCheck(name string, status doctorStatus, required bool) doctorCheck {
	return doctorCheck{
		Name:     name,
		Status:   status.String(),
		Required: required,
	}
}

func doctorCheckInstallation() doctorCheck {
	c := makeDoctorCheck("Promise installation", doctorOK, true)

	// Version
	v := version
	if v == "" {
		if epoch, err := module.CompilerEpoch(embeddedCatalog); err == nil {
			v = epoch
		} else {
			v = "unknown"
		}
	}
	c.Summary = fmt.Sprintf("Version: %s (%s-%s)", v, runtime.GOOS, runtime.GOARCH)

	// Executable path
	if execPath, err := os.Executable(); err == nil {
		c.Details = append(c.Details, "Binary: "+execPath)
	}

	// Home directory
	if home, err := module.PromiseHome(); err == nil {
		c.Details = append(c.Details, "Home: "+home)
	}

	// Embedded stdlib file count
	if entries, err := embeddedModules.ReadDir("resources/modules/std"); err == nil {
		c.Details = append(c.Details, fmt.Sprintf("Embedded stdlib: %d files", len(entries)))
	} else {
		c.Status = doctorErr.String()
		c.Summary = "Embedded stdlib not found — binary may be corrupted"
		c.Fix = "Rebuild with bin/build"
	}

	return c
}

func doctorCheckLLVM() doctorCheck {
	c := makeDoctorCheck("LLVM toolchain", doctorOK, true)

	// Determine required tools for this platform
	type toolInfo struct {
		name  string
		label string
	}
	tools := []toolInfo{
		{"opt", "opt"},
		{"llc", "llc"},
	}
	switch runtime.GOOS {
	case "darwin":
		tools = append(tools, toolInfo{"ld64.lld", "linker"})
	case "windows":
		tools = append(tools, toolInfo{"lld-link", "linker"})
	default:
		tools = append(tools, toolInfo{"ld.lld", "linker"})
	}

	if hasEmbeddedLLVM {
		c.Details = append(c.Details, "Source: embedded")
	} else {
		c.Details = append(c.Details, "Source: system PATH")
	}

	var missing []string
	for _, tool := range tools {
		path, err := findLLVMTool(tool.name)
		if err != nil {
			missing = append(missing, tool.name)
			continue
		}
		v := llvmToolVersion(path)
		if v > 0 {
			c.Details = append(c.Details, fmt.Sprintf("%s: %s (LLVM %d)", tool.label, path, v))
			if v < minLLVMMajor {
				c.Status = doctorErr.String()
				c.Summary = fmt.Sprintf("%s version %d is too old (minimum: %d)", tool.name, v, minLLVMMajor)
				c.Fix = fmt.Sprintf("Install LLVM %d+", minLLVMMajor)
			}
		} else {
			c.Details = append(c.Details, fmt.Sprintf("%s: %s", tool.label, path))
		}
	}

	if len(missing) > 0 {
		c.Status = doctorErr.String()
		c.Summary = "Missing LLVM tools: " + strings.Join(missing, ", ")
		if runtime.GOOS == "darwin" {
			c.Fix = fmt.Sprintf("brew install llvm lld (LLVM %d+)", minLLVMMajor)
		} else {
			c.Fix = fmt.Sprintf("Install LLVM %d+ or use a release build with embedded LLVM", minLLVMMajor)
		}
	} else if c.Status == doctorOK.String() {
		c.Summary = "All tools found"
	}

	return c
}

func doctorCheckMuslCRT() doctorCheck {
	c := makeDoctorCheck("musl CRT (Linux static linking)", doctorOK, true)

	target := "x86_64-unknown-linux-musl"
	if runtime.GOARCH == "arm64" {
		target = "aarch64-unknown-linux-musl"
	}

	if hasEmbeddedMuslCRT {
		c.Details = append(c.Details, "Source: embedded")
	} else {
		c.Details = append(c.Details, "Source: system")
	}

	dir, err := findMuslCRT(target)
	if err != nil {
		c.Status = doctorErr.String()
		c.Summary = "musl CRT objects not found"
		c.Fix = "Use a release build with embedded CRT, or install musl-tools"
		return c
	}

	c.Details = append(c.Details, "Path: "+dir)
	if muslCRTComplete(dir) {
		c.Summary = "All CRT objects found"
	} else {
		c.Status = doctorErr.String()
		c.Summary = "Incomplete musl CRT files in " + dir
		c.Fix = "Delete the cache directory and rebuild to re-extract CRT files"
	}

	return c
}

func doctorCheckBuildCache() doctorCheck {
	c := makeDoctorCheck("Build cache", doctorOK, false)

	dir, err := module.BuildCacheDir()
	if err != nil {
		c.Status = doctorWarn.String()
		c.Summary = "Cannot access build cache"
		c.Details = append(c.Details, err.Error())
		c.Fix = "Check PROMISE_HOME permissions"
		return c
	}

	c.Details = append(c.Details, "Path: "+dir)

	var entryCount int
	var totalSize int64
	const maxWalk = 10000
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			entryCount++
			if info, err := d.Info(); err == nil {
				totalSize += info.Size()
			}
		}
		if entryCount >= maxWalk {
			return filepath.SkipAll
		}
		return nil
	})

	sizeMB := float64(totalSize) / (1024 * 1024)
	suffix := ""
	if entryCount >= maxWalk {
		suffix = "+"
	}
	c.Summary = fmt.Sprintf("%d%s entries, %.1f MB", entryCount, suffix, sizeMB)

	return c
}

func doctorCheckModuleCache(network bool) doctorCheck {
	c := makeDoctorCheck("Module cache", doctorOK, false)

	home, err := module.PromiseHome()
	if err != nil {
		c.Status = doctorWarn.String()
		c.Summary = "Cannot determine Promise home"
		c.Fix = "Set PROMISE_HOME or ensure ~/.promise/ is accessible"
		return c
	}

	modCacheDir := filepath.Join(home, "cache", "modules")
	c.Details = append(c.Details, "Path: "+modCacheDir)

	if info, err := os.Stat(modCacheDir); err != nil {
		c.Summary = "Module cache directory does not exist (will be created on first use)"
	} else if !info.IsDir() {
		c.Status = doctorErr.String()
		c.Summary = "Module cache path exists but is not a directory"
	} else {
		c.Summary = "Module cache directory exists"
	}

	if network {
		// Simple git host reachability check
		cmd := exec.Command("git", "ls-remote", "--exit-code", "https://github.com")
		if err := cmd.Run(); err != nil {
			c.Details = append(c.Details, "Network: git host unreachable")
			if c.Status == doctorOK.String() {
				c.Status = doctorWarn.String()
			}
		} else {
			c.Details = append(c.Details, "Network: git host reachable")
		}
	}

	return c
}

func doctorCheckPromiseHome() doctorCheck {
	c := makeDoctorCheck("PROMISE_HOME", doctorOK, false)

	envVal := os.Getenv("PROMISE_HOME")
	if envVal != "" {
		c.Summary = "Set to: " + envVal
		if info, err := os.Stat(envVal); err != nil {
			c.Status = doctorWarn.String()
			c.Details = append(c.Details, "Directory does not exist")
			c.Fix = "Create the directory or unset PROMISE_HOME to use the default (~/.promise/)"
		} else if !info.IsDir() {
			c.Status = doctorErr.String()
			c.Details = append(c.Details, "Path exists but is not a directory")
			c.Fix = "Set PROMISE_HOME to a valid directory path"
		}
	} else {
		home, _ := module.PromiseHome()
		c.Summary = "Using default: " + home
	}

	return c
}

func doctorCheckJava() doctorCheck {
	c := makeDoctorCheck("Java (optional — compiler development only)", doctorOK, false)

	path, err := exec.LookPath("java")
	if err != nil {
		c.Status = doctorWarn.String()
		c.Summary = "Not found on PATH"
		c.Fix = "Install Java 11+ for ANTLR parser generation (only needed for compiler development)"
		return c
	}

	c.Summary = "Found: " + path

	// Try to get version
	cmd := exec.Command(path, "-version")
	out, err := cmd.CombinedOutput()
	if err == nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) > 0 {
			c.Details = append(c.Details, lines[0])
		}
	}

	return c
}

func doctorCheckWasmtime() doctorCheck {
	c := makeDoctorCheck("wasmtime (optional — wasm32-wasi target)", doctorOK, false)

	path, err := exec.LookPath("wasmtime")
	if err != nil {
		c.Status = doctorWarn.String()
		c.Summary = "Not found on PATH"
		c.Fix = "Install from https://wasmtime.dev/ to run wasm32-wasi binaries"
		return c
	}

	c.Summary = "Found: " + path
	if out, err := exec.Command(path, "--version").Output(); err == nil {
		c.Details = append(c.Details, strings.TrimSpace(string(out)))
	}

	return c
}

func doctorCheckNode() doctorCheck {
	c := makeDoctorCheck("node (optional — wasm32-web target tests)", doctorOK, false)

	path, err := exec.LookPath("node")
	if err != nil {
		c.Status = doctorWarn.String()
		c.Summary = "Not found on PATH"
		c.Fix = "Install Node.js to run wasm32-web tests"
		return c
	}

	c.Summary = "Found: " + path
	if out, err := exec.Command(path, "--version").Output(); err == nil {
		c.Details = append(c.Details, "Version: "+strings.TrimSpace(string(out)))
	}

	return c
}

func doctorCheckEpochs() doctorCheck {
	c := makeDoctorCheck("Epochs", doctorOK, false)

	epochs, err := module.InstalledEpochs()
	if err != nil {
		c.Status = doctorWarn.String()
		c.Summary = "Cannot list installed epochs"
		c.Details = append(c.Details, err.Error())
		c.Fix = "Check PROMISE_HOME permissions"
		return c
	}
	if len(epochs) == 0 {
		c.Summary = "No epochs installed (using embedded compiler only)"
		return c
	}

	active, _ := module.ActiveEpoch()
	c.Summary = fmt.Sprintf("%d installed", len(epochs))
	if active != "" {
		c.Details = append(c.Details, "Active: "+active)
	}
	for _, ep := range epochs {
		marker := "  "
		if ep == active {
			marker = "* "
		}
		c.Details = append(c.Details, marker+ep)
	}

	return c
}

// doctorCheckCAS verifies the content-addressed dependency cache (§4.4/§6):
// re-hash every blobs/ and archives/ entry against its content address. Because
// the fetch path trusts the cache by presence, this is the only thing that
// turns a bit-rotted / truncated CAS back into a working one. A corrupt entry
// with no --repair fails the check (Required → exit 1, CI-preflight usable);
// --repair quarantines it (recoverable, next build re-fetches) and drops the
// status to a warning so the exit code is 0.
func doctorCheckCAS(flags doctorFlags) doctorCheck {
	c := makeDoctorCheck("Dependency cache integrity", doctorOK, true)

	store, err := blobstore.NewStore()
	if err != nil {
		c.Status = doctorWarn.String()
		c.Summary = "Cannot open dependency cache"
		c.Details = append(c.Details, err.Error())
		return c
	}

	var res blobstore.VerifyResult
	if flags.repair {
		// Destructive repair (quarantine) takes the same exclusive lock as
		// install/fetch/gc so it can't race a concurrent fetch.
		unlock, lerr := store.Lock("doctor --repair")
		if lerr != nil {
			c.Status = doctorWarn.String()
			c.Summary = "Cannot lock dependency cache for repair"
			c.Details = append(c.Details, lerr.Error())
			return c
		}
		defer unlock()
		res, err = store.Verify(true)
	} else {
		res, err = store.Verify(false)
	}
	if err != nil {
		c.Status = doctorErr.String()
		c.Summary = "Could not verify dependency cache"
		c.Details = append(c.Details, err.Error())
		c.Fix = "promise doctor --repair"
		return c
	}

	c.Details = append(c.Details, fmt.Sprintf("Verified %d blobs, %d archives", res.BlobsChecked, res.ArchivesChecked))

	if res.BlobsChecked == 0 && res.ArchivesChecked == 0 {
		c.Summary = "No cached dependencies"
		return c
	}
	if len(res.Corrupt) == 0 {
		c.Summary = "All cached dependencies intact"
		return c
	}

	if flags.repair {
		c.Status = doctorWarn.String()
		c.Summary = fmt.Sprintf("Quarantined %d corrupt cache entries; next build will re-fetch clean copies", len(res.Quarantined))
		for _, ce := range res.Quarantined {
			c.Details = append(c.Details, fmt.Sprintf("quarantined %s %s", ce.Kind, ce.Hash))
		}
		return c
	}

	c.Status = doctorErr.String()
	c.Summary = fmt.Sprintf("%d corrupt cache entries detected", len(res.Corrupt))
	for _, ce := range res.Corrupt {
		c.Details = append(c.Details, fmt.Sprintf("corrupt %s %s", ce.Kind, ce.Hash))
	}
	c.Fix = "promise doctor --repair"
	return c
}

func doctorCheckXcodeCLT() doctorCheck {
	c := makeDoctorCheck("Xcode Command Line Tools", doctorOK, true)

	cmd := exec.Command("xcode-select", "-p")
	out, err := cmd.Output()
	if err != nil {
		c.Status = doctorErr.String()
		c.Summary = "Not installed"
		c.Fix = "xcode-select --install"
		return c
	}

	path := strings.TrimSpace(string(out))
	c.Summary = "Installed: " + path

	return c
}

func doctorCheckPath() doctorCheck {
	c := makeDoctorCheck("PATH", doctorOK, false)

	execPath, err := os.Executable()
	if err != nil {
		c.Status = doctorWarn.String()
		c.Summary = "Cannot determine executable path"
		return c
	}

	execDir := filepath.Clean(filepath.Dir(execPath))
	pathEnv := os.Getenv("PATH")
	pathDirs := filepath.SplitList(pathEnv)
	caseInsensitive := runtime.GOOS == "windows"

	for _, dir := range pathDirs {
		dir = filepath.Clean(dir)
		match := dir == execDir
		if !match && caseInsensitive {
			match = strings.EqualFold(dir, execDir)
		}
		if match {
			c.Summary = "promise binary directory is on PATH"
			c.Details = append(c.Details, execDir)
			return c
		}
	}

	c.Status = doctorWarn.String()
	c.Summary = "promise binary directory is not on PATH"
	c.Details = append(c.Details, "Binary at: "+execPath)
	c.Fix = fmt.Sprintf("Add %s to your PATH", execDir)

	return c
}
