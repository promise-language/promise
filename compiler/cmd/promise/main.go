package main

import (
	"context"
	"embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/antlr4-go/antlr/v4"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/codegen"
	"djabi.dev/go/promise_lang/internal/ownership"
	"djabi.dev/go/promise_lang/internal/parser"
	"djabi.dev/go/promise_lang/internal/sema"
)

//go:embed resources/std/*.pr
var embeddedStd embed.FS

// Runtime is fully codegen-emitted LLVM IR — no embedded C files needed.

func usage() {
	fmt.Fprintf(os.Stderr, `Usage: promise <command> [options] [file.pr]

Commands:
  build     Compile a Promise source file to an executable
  run       Compile and run a Promise source file
  test      Discover and run test functions
  check     Run semantic analysis (type checking)
  ast       Print the AST
  exec      Execute inline Promise code
  install   Install Promise to ~/.promise/

Options (build):
  -o <output>   Output file name (default: input file without extension)

Options (test):
  -timeout <duration>   Per-test timeout (default: 60s)
  -stress [N|duration]  Stress test mode: run repeatedly to find flaky tests
                        N = iteration count, duration = time limit, bare = until Ctrl+C

Options (exec):
  -timeout <duration>   Execution timeout (default: 60s)

Test discovery:
  promise test file.pr          Run tests in a single file
  promise test dir/             Scan directory for test files
  promise test dir/...          Scan directory recursively for test files

Inline execution:
  promise exec 'println("hello")'
  promise exec -timeout 30s 'println("hello")'
  echo 'println("hello")' | promise exec
  echo 'println("hello")' | promise
`)
}

func main() {
	if len(os.Args) < 2 {
		// If stdin is piped, treat as inline exec
		if info, err := os.Stdin.Stat(); err == nil && info.Mode()&os.ModeCharDevice == 0 {
			runExec(nil)
			return
		}
		usage()
		os.Exit(1)
	}

	cmd := os.Args[1]

	// Legacy flag-based interface for backwards compatibility
	if strings.HasPrefix(cmd, "-") {
		runLegacy(os.Args[1:])
		return
	}

	switch cmd {
	case "build":
		runBuild(os.Args[2:])
	case "run":
		runRun(os.Args[2:])
	case "test":
		runTest(os.Args[2:])
	case "check":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: promise check <file.pr>")
			os.Exit(1)
		}
		file, info := compileFrontend(os.Args[2])
		_ = file
		fmt.Printf("OK: %d types, %d objects, %d scopes\n",
			len(info.Types), len(info.Objects), len(info.Scopes))
	case "ast":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: promise ast <file.pr>")
			os.Exit(1)
		}
		file := parseSourceFile(os.Args[2])
		ast.Print(os.Stdout, file)
	case "exec":
		runExec(os.Args[2:])
	case "install":
		runInstall()
	default:
		// Try treating as a filename for backwards compatibility
		if strings.HasSuffix(cmd, ".pr") {
			runLegacy(os.Args[1:])
			return
		}
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		usage()
		os.Exit(1)
	}
}

// runLegacy handles the old flag-based interface (-ast, -check).
func runLegacy(args []string) {
	var showAST, runCheck bool
	var filename string

	for _, arg := range args {
		switch arg {
		case "-ast":
			showAST = true
		case "-check":
			runCheck = true
		default:
			filename = arg
		}
	}

	if filename == "" {
		usage()
		os.Exit(1)
	}

	if showAST {
		file := parseSourceFile(filename)
		ast.Print(os.Stdout, file)
	} else if runCheck {
		file, info := compileFrontend(filename)
		_ = file
		fmt.Printf("OK: %d types, %d objects, %d scopes\n",
			len(info.Types), len(info.Objects), len(info.Scopes))
	} else {
		// Just parse and print the parse tree
		input, err := antlr.NewFileStream(filename)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error reading %s: %v\n", filename, err)
			os.Exit(1)
		}
		lexer := parser.NewPromiseLexer(input)
		stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
		p := parser.NewPromiseParser(stream)
		tree := p.CompilationUnit()
		fmt.Println(tree.ToStringTree(nil, p))
	}
}

// runBuild compiles a .pr file to an executable.
func runBuild(args []string) {
	filename, outputFile := buildToFile(args)
	fmt.Printf("Compiled %s → %s\n", filename, outputFile)
}

// buildToFile compiles a .pr file to an executable, returning the source and output paths.
func buildToFile(args []string) (filename, outputFile string) {
	for i := 0; i < len(args); i++ {
		if args[i] == "-o" && i+1 < len(args) {
			outputFile = args[i+1]
			i++
		} else {
			filename = args[i]
		}
	}

	if filename == "" {
		fmt.Fprintln(os.Stderr, "usage: promise build [-o output] <file.pr>")
		os.Exit(1)
	}

	if outputFile == "" {
		outputFile = strings.TrimSuffix(filepath.Base(filename), ".pr")
	}

	file, info := compileFrontend(filename)
	result := codegen.Compile(file, info)

	compileAndLink(result, outputFile)
	return filename, outputFile
}

// runRun compiles and immediately runs a .pr file.
func runRun(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: promise run <file.pr>")
		os.Exit(1)
	}

	// Build to a temp file
	tmpOutput, err := os.CreateTemp("", "promise-run-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating temp file: %v\n", err)
		os.Exit(1)
	}
	tmpOutput.Close()
	defer os.Remove(tmpOutput.Name())

	// Reuse build logic
	buildArgs := []string{"-o", tmpOutput.Name()}
	buildArgs = append(buildArgs, args...)
	buildToFile(buildArgs)

	// Execute
	cmd := exec.Command(tmpOutput.Name())
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "error running: %v\n", err)
		os.Exit(1)
	}
}

// runTest discovers and runs `test annotated functions.
// Accepts a single file, a directory (non-recursive), or dir/... (recursive).
func runTest(args []string) {
	timeout := 60 * time.Second
	var stressMode bool
	var stressCount int              // 0 = unlimited
	var stressDuration time.Duration // 0 = unlimited
	var remaining []string
	for i := 0; i < len(args); i++ {
		if args[i] == "-timeout" && i+1 < len(args) {
			d, err := parseTimeoutArg(args[i+1])
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			timeout = d
			i++
		} else if args[i] == "-stress" {
			stressMode = true
			// Check if next arg is a count or duration (not a file/dir path)
			if i+1 < len(args) {
				next := args[i+1]
				if d, err := time.ParseDuration(next); err == nil {
					stressDuration = d
					i++
				} else if n, err := strconv.Atoi(next); err == nil && n > 0 {
					stressCount = n
					i++
				}
				// otherwise: bare -stress (unlimited), next arg is a target
			}
		} else {
			remaining = append(remaining, args[i])
		}
	}

	if len(remaining) < 1 {
		fmt.Fprintln(os.Stderr, "usage: promise test [-timeout duration] [-stress [N|duration]] <file.pr | dir | dir/...>")
		os.Exit(1)
	}

	target := remaining[0]

	// Check for recursive "..." pattern
	recursive := false
	if strings.HasSuffix(target, "/...") || target == "..." {
		recursive = true
		if target == "..." {
			target = "."
		} else {
			target = strings.TrimSuffix(target, "/...")
		}
	}

	// Check if target is a directory
	info, err := os.Stat(target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// Discover files
	var files []string
	if info.IsDir() {
		files = discoverTestFiles(target, recursive)
	} else {
		files = []string{target}
	}

	if stressMode {
		runStress(files, stressCount, stressDuration, timeout)
		return
	}

	if info.IsDir() {
		runTestDir(target, recursive, timeout)
	} else {
		runTestFile(target, timeout)
	}
}

// runTestFile runs test functions from a single .pr file.
func runTestFile(filename string, timeout time.Duration) {
	start := time.Now()

	// Frontend compilation (parse + merge std + sema + ownership)
	file, info := compileFrontend(filename)

	if info.HasExpectOutput {
		runE2ETest(file, info, filename, timeout, start)
		return
	}

	if len(info.Tests) == 0 {
		fmt.Println("no tests found")
		return
	}

	// Codegen
	result := codegen.Compile(file, info)

	// Generate test main (replaces user main)
	result.GenerateTestMain(info.Tests)

	// Link to temp binary (test runner is now codegen-emitted, no C files needed)
	tmpOutput, err := os.CreateTemp("", "promise-test-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating temp file: %v\n", err)
		os.Exit(1)
	}
	tmpOutput.Close()
	defer os.Remove(tmpOutput.Name())

	compileAndLink(result, tmpOutput.Name())

	// Execute test binary with timeout
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, tmpOutput.Name())
	output, runErr := cmd.CombinedOutput()
	elapsed := time.Since(start)

	if ctx.Err() == context.DeadlineExceeded {
		printTestOutput(string(output))
		fmt.Fprintf(os.Stderr, "TIMEOUT: tests exceeded %s timeout\n", timeout)
		os.Exit(1)
	}

	// Print output, replacing summary line with timed version
	summaryRe := regexp.MustCompile(`^(\d+) passed, (\d+) failed`)
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		if m := summaryRe.FindStringSubmatch(line); m != nil {
			fmt.Printf("%s passed, %s failed (%.3fs)\n", m[1], m[2], elapsed.Seconds())
		} else {
			fmt.Println(line)
		}
	}

	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "error running tests: %v\n", runErr)
		os.Exit(1)
	}
}

// runE2ETest compiles and runs a .pr file with `test(expected="..."), comparing output.
func runE2ETest(file *ast.File, info *sema.Info, filename string, timeout time.Duration, start time.Time) {
	name := strings.TrimSuffix(filepath.Base(filename), ".pr")

	// Codegen with normal main (no GenerateTestMain)
	result := codegen.Compile(file, info)

	tmpOutput, err := os.CreateTemp("", "promise-e2e-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating temp file: %v\n", err)
		os.Exit(1)
	}
	tmpOutput.Close()
	defer os.Remove(tmpOutput.Name())

	compileAndLink(result, tmpOutput.Name())

	// Execute with timeout, capturing combined stdout+stderr
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, tmpOutput.Name())
	output, err := cmd.CombinedOutput()
	elapsed := time.Since(start)

	if ctx.Err() == context.DeadlineExceeded {
		fmt.Printf("FAIL (timeout) %s\n", name)
		fmt.Printf("0 passed, 1 failed\n")
		fmt.Printf("\nFAILED:\n  %s\n", name)
		os.Exit(1)
	}

	// Compare output — non-zero exit code is NOT a failure (handles panic tests)
	actual := strings.TrimRight(string(output), "\n")
	expected := strings.TrimRight(info.ExpectOutput, "\n")

	if actual == expected {
		fmt.Printf("PASS (%.3fs) %s\n", elapsed.Seconds(), name)
		fmt.Printf("1 passed, 0 failed (%.3fs)\n", elapsed.Seconds())
	} else {
		fmt.Printf("FAIL (%.3fs) %s\n", elapsed.Seconds(), name)
		fmt.Printf("  expected: %s\n", firstLines(expected, 3))
		fmt.Printf("  actual:   %s\n", firstLines(actual, 3))
		if err != nil {
			fmt.Printf("  exit:     %v\n", err)
		}
		fmt.Printf("0 passed, 1 failed (%.3fs)\n", elapsed.Seconds())
		fmt.Printf("\nFAILED:\n  %s\n", name)
		os.Exit(1)
	}
}

// firstLines returns the first n lines of s, joined by " | ".
func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
		lines = append(lines, "...")
	}
	return strings.Join(lines, " | ")
}

// runTestDir discovers .pr files in a directory and runs tests from each.
func runTestDir(dir string, recursive bool, timeout time.Duration) {
	totalStart := time.Now()

	files := discoverTestFiles(dir, recursive)
	if len(files) == 0 {
		fmt.Println("no test files found")
		return
	}

	selfExe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot determine executable path: %v\n", err)
		os.Exit(1)
	}

	summaryRe := regexp.MustCompile(`^(\d+) passed, (\d+) failed`)
	failLineRe := regexp.MustCompile(`^FAIL \(\d+\.\d+s\) (.+)$`)

	totalPassed := 0
	totalFailed := 0
	totalFiles := 0
	failedFiles := 0
	var failures []string

	for _, f := range files {
		// Run "promise test <file>" as subprocess with timeout
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		cmd := exec.CommandContext(ctx, selfExe, "test", "-timeout", fmt.Sprintf("%gs", timeout.Seconds()), f)
		output, err := cmd.CombinedOutput()
		timedOut := ctx.Err() == context.DeadlineExceeded
		cancel()
		outStr := strings.TrimSpace(string(output))

		// Skip files with no tests
		if !timedOut && outStr == "no tests found" {
			continue
		}

		totalFiles++

		// Print file header and output
		relPath, relErr := filepath.Rel(dir, f)
		if relErr != nil {
			relPath = f
		}
		fmt.Printf("--- %s ---\n", relPath)

		if timedOut {
			fmt.Printf("TIMEOUT: exceeded %s timeout\n", timeout)
			failedFiles++
			totalFailed++
			failures = append(failures, relPath+" (timeout)")
			continue
		}

		if err != nil {
			// Compilation or runtime error
			printTestOutput(outStr)
			failedFiles++

			// Try to parse summary from output
			if m := summaryRe.FindStringSubmatch(lastLine(outStr)); m != nil {
				passed := atoi(m[1])
				failed := atoi(m[2])
				totalPassed += passed
				totalFailed += failed
			} else {
				// Count entire file as one failure
				totalFailed++
			}

			// Extract individual FAIL lines from output
			foundFailLines := false
			for _, line := range strings.Split(outStr, "\n") {
				if m := failLineRe.FindStringSubmatch(line); m != nil {
					failures = append(failures, relPath+": "+m[1])
					foundFailLines = true
				}
			}
			if !foundFailLines {
				failures = append(failures, relPath+" (compilation error)")
			}
			continue
		}

		// Parse summary before printing
		if m := summaryRe.FindStringSubmatch(lastLine(outStr)); m != nil {
			totalPassed += atoi(m[1])
			totalFailed += atoi(m[2])
		}

		// Print test output (strip the summary line and empty lines)
		for _, line := range strings.Split(outStr, "\n") {
			if line == "" || summaryRe.MatchString(line) {
				continue
			}
			fmt.Println(line)
		}
	}

	if totalFiles == 0 {
		fmt.Println("no test files found")
		return
	}

	// Print grand summary
	totalElapsed := time.Since(totalStart)
	fmt.Printf("%d passed, %d failed (%d files, %.3fs)\n", totalPassed, totalFailed, totalFiles, totalElapsed.Seconds())

	// Print failure list if any
	if len(failures) > 0 {
		fmt.Printf("\nFAILED:\n")
		for _, f := range failures {
			fmt.Printf("  %s\n", f)
		}
	}

	if totalFailed > 0 || failedFiles > 0 {
		os.Exit(1)
	}
}

// discoverTestFiles finds .pr files in a directory.
func discoverTestFiles(dir string, recursive bool) []string {
	var files []string

	if recursive {
		if err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if !d.IsDir() && strings.HasSuffix(d.Name(), ".pr") {
				files = append(files, path)
			}
			return nil
		}); err != nil {
			fmt.Fprintf(os.Stderr, "error walking directory: %v\n", err)
			os.Exit(1)
		}
	} else {
		entries, err := os.ReadDir(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error reading directory: %v\n", err)
			os.Exit(1)
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".pr") {
				files = append(files, filepath.Join(dir, e.Name()))
			}
		}
	}

	sort.Strings(files)
	return files
}

// printTestOutput prints a test output string, skipping empty lines.
func printTestOutput(s string) {
	for _, line := range strings.Split(s, "\n") {
		if line != "" {
			fmt.Println(line)
		}
	}
}

// lastLine returns the last non-empty line of a string.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i] != "" {
			return lines[i]
		}
	}
	return ""
}

// atoi converts a string to int, returning 0 on failure.
func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// parseTimeoutArg parses a timeout string as a Go duration ("60s", "2m") or plain seconds ("60").
func parseTimeoutArg(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err == nil {
		return d, nil
	}
	secs, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid timeout: %s (use duration like '60s' or seconds like '60')", s)
	}
	return time.Duration(secs) * time.Second, nil
}

// compileAndLink writes the IR to a temp file and links it into the output binary.
// On Linux and macOS, uses opt + llc + linker pipeline (Phase 7b/7c).
// On other platforms (or with PROMISE_USE_CLANG=1), uses clang as driver.
func compileAndLink(result *codegen.CompileResult, outputFile string) {
	llFile, err := os.CreateTemp("", "promise-*.ll")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating temp file: %v\n", err)
		os.Exit(1)
	}
	defer os.Remove(llFile.Name())

	// Dump generated LLVM IR to a file for debugging/inspection.
	// Usage: PROMISE_DUMP_IR=/tmp/out.ll promise build foo.pr
	if envDump := os.Getenv("PROMISE_DUMP_IR"); envDump != "" {
		_ = os.WriteFile(envDump, []byte(result.Module.String()), 0644)
	}

	if _, err := fmt.Fprint(llFile, result.Module.String()); err != nil {
		fmt.Fprintf(os.Stderr, "error writing IR: %v\n", err)
		os.Exit(1)
	}
	llFile.Close()

	target := codegen.HostTargetTriple()

	if useClangPipeline(target) {
		compileAndLinkClang(llFile.Name(), target, outputFile)
	} else {
		compileAndLinkLLVM(llFile.Name(), target, outputFile)
	}
}

// useClangPipeline returns true if the clang pipeline should be used instead of opt+llc+linker.
func useClangPipeline(target string) bool {
	if os.Getenv("PROMISE_USE_CLANG") == "1" {
		return true
	}
	// Linux and macOS use the LLVM pipeline. Other platforms use clang.
	return !strings.Contains(target, "linux") && !strings.Contains(target, "macosx")
}

// minLLVMMajor is the minimum LLVM/clang major version required.
// LLVM 22+ is required: llvm.coro.end returns void (changed from i1 in LLVM 20-21).
// Applies to both the opt/llc/linker pipeline and the clang fallback.
const minLLVMMajor = 22

// maxLLVMSearch is the highest LLVM version to probe when searching PATH for
// versioned tool names (e.g. opt-25, opt-24, ..., opt-20).
const maxLLVMSearch = 25

// findClang returns the path to a clang binary.
// Prefers Homebrew LLVM over Apple clang.
func findClang() string {
	// Check PROMISE_CLANG env override first
	if p := os.Getenv("PROMISE_CLANG"); p != "" {
		return p
	}
	// Homebrew LLVM (macOS)
	for _, p := range []string{
		"/opt/homebrew/opt/llvm/bin/clang",
		"/usr/local/opt/llvm/bin/clang",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "clang"
}

// clangVersion returns the major version of the given clang binary, or 0 if it cannot be determined.
// Handles: "clang version X" (upstream), "Apple clang version X" (Xcode), "Ubuntu clang version X" (apt).
func clangVersion(clangPath string) int {
	out, err := exec.Command(clangPath, "--version").Output()
	if err != nil {
		return 0
	}
	re := regexp.MustCompile(`clang version (\d+)`)
	m := re.FindSubmatch(out)
	if m == nil {
		return 0
	}
	v, err := strconv.Atoi(string(m[1]))
	if err != nil {
		return 0
	}
	return v
}

// checkClangVersion verifies the clang binary meets the minimum version requirement.
func checkClangVersion(clangPath string) {
	v := clangVersion(clangPath)
	if v == 0 {
		return // can't determine version, let clang errors speak for themselves
	}
	if v < minLLVMMajor {
		fmt.Fprintf(os.Stderr, "error: clang version %d is too old (minimum required: %d)\n", v, minLLVMMajor)
		fmt.Fprintf(os.Stderr, "  clang path: %s\n", clangPath)
		fmt.Fprintf(os.Stderr, "  install clang %d+ or set PROMISE_CLANG to override\n", minLLVMMajor)
		os.Exit(1)
	}
}

// --- LLVM tool pipeline ---

// findLLVMTool locates an LLVM tool (opt, llc, ld.lld, ld64.lld) by searching:
// 1. Sibling directory of the promise binary
// 2. Environment variable override (PROMISE_OPT, PROMISE_LLC, PROMISE_LLD, PROMISE_LD64LLD)
// 3. Homebrew LLVM (macOS)
// 4. Versioned names on PATH (e.g., opt-22, llc-22, ld.lld-22) from newest to minLLVMMajor
// 5. Unversioned names on PATH (e.g., opt, llc, ld.lld)
func findLLVMTool(name string) (string, error) {
	envMap := map[string]string{
		"opt":      "PROMISE_OPT",
		"llc":      "PROMISE_LLC",
		"ld.lld":   "PROMISE_LLD",
		"ld64.lld": "PROMISE_LD64LLD",
	}

	// 1. Sibling of promise binary
	if execPath, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(execPath), name)
		if _, err := os.Stat(sibling); err == nil {
			return sibling, nil
		}
	}

	// 2. Env override
	if envName, ok := envMap[name]; ok {
		if p := os.Getenv(envName); p != "" {
			return p, nil
		}
	}

	// 3. Homebrew LLVM (macOS only)
	if runtime.GOOS == "darwin" {
		for _, prefix := range []string{
			"/opt/homebrew/opt/llvm/bin",
			"/usr/local/opt/llvm/bin",
		} {
			p := filepath.Join(prefix, name)
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		}
	}

	// 4. Versioned names on PATH (try newest to oldest)
	for v := maxLLVMSearch; v >= minLLVMMajor; v-- {
		versioned := fmt.Sprintf("%s-%d", name, v)
		if path, err := exec.LookPath(versioned); err == nil {
			return path, nil
		}
	}

	// 5. Unversioned on PATH
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}

	envName := envMap[name]
	return "", fmt.Errorf("%s not found\n  searched: sibling of promise binary, $%s, Homebrew LLVM, PATH (%s-{%d..%d}, %s)\n  install LLVM %d+ or set PROMISE_USE_CLANG=1 to use clang",
		name, envName, name, maxLLVMSearch, minLLVMMajor, name, minLLVMMajor)
}

// llvmToolVersion returns the major version of an LLVM tool, or 0 if it cannot be determined.
// Handles different version formats:
//   - opt/llc: "LLVM version 22.1.2"
//   - ld.lld:  "LLD 22.1.2" (no "LLVM version" prefix)
func llvmToolVersion(toolPath string) int {
	out, err := exec.Command(toolPath, "--version").Output()
	if err != nil {
		return 0
	}
	// Try "LLVM version X" first (opt, llc)
	re := regexp.MustCompile(`LLVM version (\d+)`)
	if m := re.FindSubmatch(out); m != nil {
		v, _ := strconv.Atoi(string(m[1]))
		return v
	}
	// Try "LLD X.Y.Z" (ld.lld)
	re2 := regexp.MustCompile(`LLD (\d+)`)
	if m := re2.FindSubmatch(out); m != nil {
		v, _ := strconv.Atoi(string(m[1]))
		return v
	}
	return 0
}

// checkLLVMToolVersion verifies an LLVM tool meets the minimum version requirement.
func checkLLVMToolVersion(toolPath string) {
	v := llvmToolVersion(toolPath)
	if v == 0 {
		return
	}
	if v < minLLVMMajor {
		fmt.Fprintf(os.Stderr, "error: LLVM version %d is too old (minimum required: %d)\n", v, minLLVMMajor)
		fmt.Fprintf(os.Stderr, "  tool path: %s\n", toolPath)
		fmt.Fprintf(os.Stderr, "  install LLVM %d+ or set PROMISE_USE_CLANG=1 to use clang\n", minLLVMMajor)
		os.Exit(1)
	}
}

// crtInfo holds discovered CRT object paths for Linux linking.
type crtInfo struct {
	scrt1     string   // Scrt1.o — PIE startup entry point
	crti      string   // crti.o — .init/.fini section prologue
	crtn      string   // crtn.o — .init/.fini section epilogue
	crtbeginS string   // crtbeginS.o — GCC PIC constructor registration
	crtendS   string   // crtendS.o — GCC PIC destructor cleanup
	libDirs   []string // -L library search paths
}

// findCRT discovers system CRT objects on Linux.
// Primary: cc -print-file-name=X. Fallback: probe common paths.
func findCRT(target string) (*crtInfo, error) {
	info := &crtInfo{}

	type crtFile struct {
		name string
		dest *string
	}
	files := []crtFile{
		{"Scrt1.o", &info.scrt1},
		{"crti.o", &info.crti},
		{"crtn.o", &info.crtn},
		{"crtbeginS.o", &info.crtbeginS},
		{"crtendS.o", &info.crtendS},
	}

	// Find a system C compiler for -print-file-name
	ccPath := ""
	for _, name := range []string{"cc", "gcc"} {
		if p, err := exec.LookPath(name); err == nil {
			ccPath = p
			break
		}
	}

	var missing []string
	if ccPath != "" {
		for _, f := range files {
			out, err := exec.Command(ccPath, "-print-file-name="+f.name).Output()
			if err != nil {
				missing = append(missing, f.name)
				continue
			}
			path := strings.TrimSpace(string(out))
			// cc returns just the filename if it can't find the file
			if path == f.name || path == "" {
				missing = append(missing, f.name)
				continue
			}
			absPath, err := filepath.Abs(path)
			if err != nil {
				absPath = path
			}
			if _, err := os.Stat(absPath); err != nil {
				missing = append(missing, f.name)
				continue
			}
			*f.dest = absPath
		}
	} else {
		for _, f := range files {
			missing = append(missing, f.name)
		}
	}

	// Fallback: probe common paths for any missing files
	if len(missing) > 0 {
		tryCRTFallback(info, missing, target)
	}

	// Check all found
	var stillMissing []string
	for _, f := range files {
		if *f.dest == "" {
			stillMissing = append(stillMissing, f.name)
		}
	}
	if len(stillMissing) > 0 {
		return nil, fmt.Errorf("CRT objects not found: %s\n  install build-essential (Debian/Ubuntu) or gcc (Fedora/Arch)\n  or set PROMISE_USE_CLANG=1 to use clang",
			strings.Join(stillMissing, ", "))
	}

	// Derive library search paths from CRT locations
	seen := map[string]bool{}
	addDir := func(path string) {
		dir := filepath.Dir(path)
		if !seen[dir] {
			seen[dir] = true
			info.libDirs = append(info.libDirs, dir)
		}
	}
	addDir(info.crti)
	addDir(info.crtbeginS)

	// Add standard library paths
	arch := "x86_64"
	if strings.HasPrefix(target, "aarch64") {
		arch = "aarch64"
	}
	for _, dir := range []string{
		"/lib/" + arch + "-linux-gnu",
		"/usr/lib/" + arch + "-linux-gnu",
		"/lib64",
		"/usr/lib64",
	} {
		if _, err := os.Stat(dir); err == nil && !seen[dir] {
			seen[dir] = true
			info.libDirs = append(info.libDirs, dir)
		}
	}

	return info, nil
}

// tryCRTFallback probes common Linux paths for missing CRT objects.
func tryCRTFallback(info *crtInfo, missing []string, target string) {
	arch := "x86_64"
	if strings.HasPrefix(target, "aarch64") {
		arch = "aarch64"
	}

	// glibc CRT dirs
	glibcDirs := []string{
		"/lib/" + arch + "-linux-gnu",
		"/usr/lib/" + arch + "-linux-gnu",
		"/lib64",
		"/usr/lib64",
		"/usr/lib",
	}

	// GCC CRT dirs (versioned)
	var gccDirs []string
	for _, base := range []string{
		"/usr/lib/gcc/" + arch + "-linux-gnu",
	} {
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				gccDirs = append(gccDirs, filepath.Join(base, e.Name()))
			}
		}
	}

	for _, name := range missing {
		var dest *string
		var searchDirs []string
		switch name {
		case "Scrt1.o":
			dest = &info.scrt1
			searchDirs = glibcDirs
		case "crti.o":
			dest = &info.crti
			searchDirs = glibcDirs
		case "crtn.o":
			dest = &info.crtn
			searchDirs = glibcDirs
		case "crtbeginS.o":
			dest = &info.crtbeginS
			searchDirs = gccDirs
		case "crtendS.o":
			dest = &info.crtendS
			searchDirs = gccDirs
		}
		if dest == nil {
			continue
		}
		for _, dir := range searchDirs {
			path := filepath.Join(dir, name)
			if _, err := os.Stat(path); err == nil {
				*dest = path
				break
			}
		}
	}
}

// --- macOS (Phase 7c) ---

// macOSSDKInfo holds discovered macOS SDK information for linking.
type macOSSDKInfo struct {
	sysroot    string // SDK path from xcrun --show-sdk-path
	sdkVersion string // SDK version from xcrun --show-sdk-version (e.g. "15.2")
}

// findMacOSSDK discovers the macOS SDK sysroot via xcrun.
func findMacOSSDK() (*macOSSDKInfo, error) {
	sysroot, err := exec.Command("xcrun", "--show-sdk-path").Output()
	if err != nil {
		return nil, fmt.Errorf("macOS SDK not found: xcrun --show-sdk-path failed\n  install Xcode CommandLineTools: xcode-select --install")
	}
	sysrootPath := strings.TrimSpace(string(sysroot))
	if sysrootPath == "" {
		return nil, fmt.Errorf("macOS SDK not found: xcrun returned empty path\n  install Xcode CommandLineTools: xcode-select --install")
	}
	if _, err := os.Stat(sysrootPath); err != nil {
		return nil, fmt.Errorf("macOS SDK path does not exist: %s\n  install Xcode CommandLineTools: xcode-select --install", sysrootPath)
	}

	info := &macOSSDKInfo{sysroot: sysrootPath}
	if sdkVer, err := exec.Command("xcrun", "--show-sdk-version").Output(); err == nil {
		info.sdkVersion = strings.TrimSpace(string(sdkVer))
	}
	return info, nil
}

// darwinTripleInfo holds parsed components of a macOS target triple.
type darwinTripleInfo struct {
	arch       string // "arm64" or "x86_64"
	minVersion string // deployment target version, e.g. "14.0.0"
}

// parseDarwinTriple extracts architecture and version from a macOS target triple.
// Example: "arm64-apple-macosx14.0.0" → arch="arm64", minVersion="14.0.0"
func parseDarwinTriple(target string) darwinTripleInfo {
	info := darwinTripleInfo{arch: "arm64", minVersion: "14.0.0"}
	if strings.HasPrefix(target, "x86_64") {
		info.arch = "x86_64"
	}
	if idx := strings.Index(target, "macosx"); idx >= 0 {
		if ver := target[idx+len("macosx"):]; ver != "" {
			info.minVersion = ver
		}
	}
	return info
}

// findDarwinLinker returns the path to a Mach-O linker for macOS.
// Tries ld64.lld first (for bundled release), then falls back to system ld.
// Returns (path, isLLD, error).
func findDarwinLinker() (string, bool, error) {
	// 1. Try ld64.lld via standard LLVM tool discovery
	if path, err := findLLVMTool("ld64.lld"); err == nil {
		return path, true, nil
	}

	// 2. Environment override for system ld
	if p := os.Getenv("PROMISE_LD"); p != "" {
		return p, false, nil
	}

	// 3. System ld (always available on macOS with CommandLineTools)
	if path, err := exec.LookPath("ld"); err == nil {
		return path, false, nil
	}

	return "", false, fmt.Errorf("no Mach-O linker found\n  install Xcode CommandLineTools: xcode-select --install\n  or set PROMISE_USE_CLANG=1 to use clang")
}

// isDarwinTarget returns true if the target triple is macOS/Darwin.
func isDarwinTarget(target string) bool {
	return strings.Contains(target, "macosx")
}

// buildDarwinLinkArgs builds the linker argument list for macOS Mach-O linking.
// Works with both ld64.lld and Apple's system ld.
func buildDarwinLinkArgs(target, objFile, outputFile string) []string {
	sdk, err := findMacOSSDK()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	tri := parseDarwinTriple(target)

	// Use SDK version for -platform_version if available, otherwise deployment target.
	sdkVersion := tri.minVersion
	if sdk.sdkVersion != "" {
		sdkVersion = sdk.sdkVersion
	}

	return []string{
		"-arch", tri.arch,
		"-platform_version", "macos", tri.minVersion, sdkVersion,
		"-syslibroot", sdk.sysroot,
		"-o", outputFile,
		objFile,
		"-lSystem",
	}
}

// --- Linux linking ---

// dynamicLinker returns the ELF dynamic linker path for the given target.
func dynamicLinker(target string) string {
	if strings.HasPrefix(target, "aarch64") {
		return "/lib/ld-linux-aarch64.so.1"
	}
	return "/lib64/ld-linux-x86-64.so.2"
}

// emulationMode returns the ld.lld -m flag for the given target.
func emulationMode(target string) string {
	if strings.HasPrefix(target, "aarch64") {
		return "aarch64linux"
	}
	return "elf_x86_64"
}

// buildLinuxLinkArgs builds the ld.lld argument list for dynamic glibc ELF linking.
func buildLinuxLinkArgs(target, objFile, outputFile string) []string {
	crt, err := findCRT(target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	args := []string{
		"-z", "relro",
		"--hash-style=gnu",
		"--build-id",
		"--eh-frame-hdr",
		"-m", emulationMode(target),
		"-pie",
		"-dynamic-linker", dynamicLinker(target),
		"-o", outputFile,
		// CRT startup (order matters)
		crt.scrt1,
		crt.crti,
		crt.crtbeginS,
	}

	// Library search paths
	for _, dir := range crt.libDirs {
		args = append(args, "-L"+dir)
	}

	// Object file
	args = append(args, objFile)

	// Libraries (matches clang's link order)
	args = append(args,
		"-lpthread",
		"-lgcc", "--as-needed", "-lgcc_s", "--no-as-needed",
		"-lc",
		"-lgcc", "--as-needed", "-lgcc_s", "--no-as-needed",
	)

	// CRT finalization (order matters)
	args = append(args, crt.crtendS, crt.crtn)

	return args
}

// --- Musl CRT (Phase 7b') ---

// muslCRTFiles lists the musl CRT objects needed for static linking.
var muslCRTFiles = []string{"crt1.o", "crti.o", "crtn.o", "libc.a"}

// muslArchDir returns the CRT subdirectory name for the given target triple.
func muslArchDir(target string) string {
	if strings.HasPrefix(target, "aarch64") {
		return "aarch64-linux-musl"
	}
	return "x86_64-linux-musl"
}

// muslCRTComplete checks if all required musl CRT files exist in dir.
func muslCRTComplete(dir string) bool {
	for _, name := range muslCRTFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return false
		}
	}
	return true
}

// muslCRTValid checks if cached musl CRT files match the embedded versions (by size).
// Uses fs.DirEntry.Info() to compare sizes without reading file contents into memory.
func muslCRTValid(dir string) bool {
	if !hasEmbeddedMuslCRT {
		return muslCRTComplete(dir)
	}
	arch := filepath.Base(dir)
	prefix := "resources/crt/" + arch

	// Build a size map from the embedded FS
	entries, err := embeddedMuslCRT.ReadDir(prefix)
	if err != nil {
		return false
	}
	embeddedSizes := make(map[string]int64, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			return false
		}
		embeddedSizes[e.Name()] = info.Size()
	}

	for _, name := range muslCRTFiles {
		cached, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			return false
		}
		embSize, ok := embeddedSizes[name]
		if !ok {
			return false
		}
		if cached.Size() != embSize {
			return false
		}
	}
	return true
}

// findMuslCRT locates musl CRT objects for static linking.
// Discovery order:
// 1. Sibling of promise binary: {exe_dir}/crt/{arch}/
// 2. Installed location: ~/.promise/lib/crt/{arch}/
// 3. Cache dir: ~/.promise/cache/crt/{arch}/
// 4. Extract embedded CRT to cache (first build only)
func findMuslCRT(target string) (string, error) {
	arch := muslArchDir(target)

	// 1. Sibling of promise binary
	if execPath, err := os.Executable(); err == nil {
		dir := filepath.Join(filepath.Dir(execPath), "crt", arch)
		if muslCRTComplete(dir) {
			return dir, nil
		}
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %v", err)
	}

	// 2. Installed location (~/.promise/lib/crt/{arch}/)
	installDir := filepath.Join(homeDir, ".promise", "lib", "crt", arch)
	if muslCRTComplete(installDir) {
		return installDir, nil
	}

	// 3. Cache dir (~/.promise/cache/crt/{arch}/)
	cacheDir := filepath.Join(homeDir, ".promise", "cache", "crt", arch)

	if muslCRTValid(cacheDir) {
		return cacheDir, nil
	}

	// 4. Extract embedded CRT to cache
	if !hasEmbeddedMuslCRT {
		return "", fmt.Errorf("musl CRT not available for %s\n  this binary was not built with embedded musl CRT\n  set PROMISE_USE_CLANG=1 to use clang with system glibc instead", arch)
	}

	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", fmt.Errorf("cannot create CRT cache dir %s: %v", cacheDir, err)
	}

	prefix := "resources/crt/" + arch
	for _, name := range muslCRTFiles {
		data, err := embeddedMuslCRT.ReadFile(prefix + "/" + name)
		if err != nil {
			return "", fmt.Errorf("cannot read embedded %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(cacheDir, name), data, 0644); err != nil {
			return "", fmt.Errorf("cannot write %s to cache: %v", name, err)
		}
	}

	return cacheDir, nil
}

// buildMuslLinkArgs builds the ld.lld argument list for static musl linking.
func buildMuslLinkArgs(target, objFile, outputFile, crtDir string) []string {
	return []string{
		"-m", emulationMode(target),
		"-static",
		"--build-id",
		"--eh-frame-hdr",
		"--gc-sections",
		"-o", outputFile,
		filepath.Join(crtDir, "crt1.o"),
		filepath.Join(crtDir, "crti.o"),
		objFile,
		filepath.Join(crtDir, "libc.a"),
		filepath.Join(crtDir, "crtn.o"),
	}
}

// compileAndLinkLLVM runs the opt + llc + linker pipeline.
// Linux: opt → llc → ld.lld. macOS: opt → llc → ld64.lld (or system ld).
func compileAndLinkLLVM(llFile, target, outputFile string) {
	optPath, err := findLLVMTool("opt")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	llcPath, err := findLLVMTool("llc")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	checkLLVMToolVersion(optPath)
	checkLLVMToolVersion(llcPath)

	// Step 1: opt -O1 (optimization + coroutine passes CoroSplit/CoroElide)
	bcFile, err := os.CreateTemp("", "promise-*.bc")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating temp file: %v\n", err)
		os.Exit(1)
	}
	bcFile.Close()
	defer os.Remove(bcFile.Name())

	optCmd := exec.Command(optPath, "-O1", llFile, "-o", bcFile.Name())
	optCmd.Stderr = os.Stderr
	if err := optCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error running opt: %v\n", err)
		os.Exit(1)
	}

	// Step 2: llc (bitcode → object file)
	objFile, err := os.CreateTemp("", "promise-*.o")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating temp file: %v\n", err)
		os.Exit(1)
	}
	objFile.Close()
	defer os.Remove(objFile.Name())

	llcCmd := exec.Command(llcPath,
		"-mtriple="+target,
		"-filetype=obj",
		"-relocation-model=pic",
		bcFile.Name(),
		"-o", objFile.Name(),
	)
	llcCmd.Stderr = os.Stderr
	if err := llcCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error running llc: %v\n", err)
		os.Exit(1)
	}

	// Step 3: Link (platform-specific)
	if isDarwinTarget(target) {
		linkDarwin(objFile.Name(), target, outputFile)
	} else {
		linkLinux(objFile.Name(), target, outputFile)
	}
}

// linkDarwin runs ld64.lld or system ld for macOS Mach-O linking.
func linkDarwin(objFile, target, outputFile string) {
	linkerPath, isLLD, err := findDarwinLinker()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if isLLD {
		checkLLVMToolVersion(linkerPath)
	}

	linkArgs := buildDarwinLinkArgs(target, objFile, outputFile)
	linkCmd := exec.Command(linkerPath, linkArgs...)
	linkCmd.Stderr = os.Stderr
	if err := linkCmd.Run(); err != nil {
		linkerName := "ld"
		if isLLD {
			linkerName = "ld64.lld"
		}
		fmt.Fprintf(os.Stderr, "error linking (%s): %v\n", linkerName, err)
		os.Exit(1)
	}
}

// linkLinux runs ld.lld for Linux ELF linking (glibc or musl).
func linkLinux(objFile, target, outputFile string) {
	lldPath, err := findLLVMTool("ld.lld")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	checkLLVMToolVersion(lldPath)

	var linkArgs []string
	if strings.Contains(target, "linux-musl") {
		crtDir, err := findMuslCRT(target)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		linkArgs = buildMuslLinkArgs(target, objFile, outputFile, crtDir)
	} else {
		linkArgs = buildLinuxLinkArgs(target, objFile, outputFile)
	}
	linkCmd := exec.Command(lldPath, linkArgs...)
	linkCmd.Stderr = os.Stderr
	if err := linkCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error linking (ld.lld): %v\n", err)
		os.Exit(1)
	}
}

// compileAndLinkClang runs the clang pipeline (non-Linux or fallback).
func compileAndLinkClang(llFile, target, outputFile string) {
	linkArgs := []string{"-O1", "-target", target, llFile, "-o", outputFile}
	if strings.Contains(target, "linux") {
		linkArgs = append(linkArgs, "-lpthread")
	}
	clang := findClang()
	checkClangVersion(clang)
	linkCmd := exec.Command(clang, linkArgs...)
	linkCmd.Stderr = os.Stderr
	if err := linkCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error linking (clang): %v\n", err)
		os.Exit(1)
	}
}

// --- Frontend pipeline ---

// parseFile parses a .pr file and returns the AST.
func parseSourceFile(filename string) *ast.File {
	input, err := antlr.NewFileStream(filename)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading %s: %v\n", filename, err)
		os.Exit(1)
	}

	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	lexEl := &errorListener{filename: filename}
	lexer.AddErrorListener(lexEl)

	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := parser.NewPromiseParser(stream)
	p.RemoveErrorListeners()
	parseEl := &errorListener{filename: filename}
	p.AddErrorListener(parseEl)

	tree := p.CompilationUnit()

	if lexEl.errors+parseEl.errors > 0 {
		os.Exit(1)
	}

	file, errs := ast.Build(filename, tree)
	if len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintln(os.Stderr, e)
		}
		os.Exit(1)
	}
	return file
}

// compileFrontend runs the full frontend pipeline: parse → merge std → sema → ownership.
func compileFrontend(filename string) (*ast.File, *sema.Info) {
	file := parseSourceFile(filename)

	// Merge standard library declarations
	stdDir := findStdDir()
	if stdDir != "" {
		stdFiles := parseStdFiles(stdDir)
		file = mergeStdDecls(file, stdFiles)
	}

	info, errs := sema.Check(file)
	if len(errs) > 0 {
		printFileErrors(filename, errs)
		os.Exit(1)
	}

	ownerErrs := ownership.Check(file, info)
	if len(ownerErrs) > 0 {
		printFileErrors(filename, ownerErrs)
		os.Exit(1)
	}

	return file, info
}

// findStdDir searches for the std/ directory containing standard library .pr files.
func findStdDir() string {
	candidates := []string{
		"std",
		"../std",
		"../../std",
	}

	// Check relative to executable
	if execPath, err := os.Executable(); err == nil {
		dir := filepath.Dir(execPath)
		candidates = append(candidates,
			filepath.Join(dir, "std"),
			filepath.Join(dir, "..", "std"),
			filepath.Join(dir, "..", "..", "std"),
		)
	}

	// Check installed location
	if homeDir, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(homeDir, ".promise", "lib", "std"),
		)
	}

	for _, c := range candidates {
		info, err := os.Stat(c)
		if err == nil && info.IsDir() {
			abs, err := filepath.Abs(c)
			if err == nil {
				return abs
			}
			return c
		}
	}
	return ""
}

// parseStdFiles parses all .pr files in the std directory.
// TODO: OS errors (unreadable dir) and parse errors in std files silently return nil — add error reporting
func parseStdFiles(stdDir string) []*ast.File {
	entries, err := os.ReadDir(stdDir)
	if err != nil {
		return nil
	}
	var files []*ast.File
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".pr") {
			f := parseSourceFile(filepath.Join(stdDir, e.Name()))
			files = append(files, f)
		}
	}
	return files
}

// mergeStdDecls prepends std library declarations into the user file, tagging them with IsStd.
func mergeStdDecls(userFile *ast.File, stdFiles []*ast.File) *ast.File {
	var stdDecls []ast.Decl
	for _, sf := range stdFiles {
		for _, d := range sf.Decls {
			// Tag each declaration as coming from std
			switch decl := d.(type) {
			case *ast.FuncDecl:
				decl.IsStd = true
			case *ast.TypeDecl:
				decl.IsStd = true
			case *ast.EnumDecl:
				decl.IsStd = true // TODO: no std enums exist yet; add test when one is added
			}
			stdDecls = append(stdDecls, d)
		}
	}

	// Prepend std declarations before user declarations
	merged := make([]ast.Decl, 0, len(stdDecls)+len(userFile.Decls))
	merged = append(merged, stdDecls...)
	merged = append(merged, userFile.Decls...)
	userFile.Decls = merged
	return userFile
}

type errorListener struct {
	antlr.DefaultErrorListener
	filename string
	source   string // non-empty for inline mode: show source context
	wrapped  bool   // if true, adjust line numbers by -1
	errors   int
}

func (l *errorListener) SyntaxError(
	_ antlr.Recognizer,
	_ interface{},
	line, column int,
	msg string,
	_ antlr.RecognitionException,
) {
	if l.source != "" {
		lines := strings.Split(l.source, "\n")
		displayLine := line
		if l.wrapped {
			displayLine--
		}
		fmt.Fprintf(os.Stderr, "%d:%d: %s\n", displayLine, column, msg)
		printErrorContext(lines, line-1, column)
	} else {
		fmt.Fprintf(os.Stderr, "%s:%d:%d: %s\n", l.filename, line, column, msg)
		lines := readFileLines(l.filename)
		if lines != nil {
			printErrorContext(lines, line-1, column)
		}
	}
	l.errors++
}

// --- Inline execution ---

// runExec executes inline Promise code from CLI arguments or stdin.
func runExec(args []string) {
	timeout := 60 * time.Second
	var remaining []string
	for i := 0; i < len(args); i++ {
		if args[i] == "-timeout" && i+1 < len(args) {
			d, err := parseTimeoutArg(args[i+1])
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			timeout = d
			i++
		} else {
			remaining = append(remaining, args[i])
		}
	}

	var source string

	if len(remaining) > 0 {
		source = strings.Join(remaining, " ")
	} else {
		// Read from stdin
		info, err := os.Stdin.Stat()
		if err != nil || info.Mode()&os.ModeCharDevice != 0 {
			fmt.Fprintln(os.Stderr, "usage: promise exec <code>")
			fmt.Fprintln(os.Stderr, "       echo '<code>' | promise exec")
			os.Exit(1)
		}
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error reading stdin: %v\n", err)
			os.Exit(1)
		}
		source = string(data)
	}

	source = strings.TrimSpace(source)
	if source == "" {
		fmt.Fprintln(os.Stderr, "error: no code provided")
		os.Exit(1)
	}

	// Wrap in main() if needed
	wrapped := false
	if !isFullFile(source) {
		if !strings.HasSuffix(source, ";") && !strings.HasSuffix(source, "}") {
			source += ";"
		}
		source = "main() {\n" + source + "\n}"
		wrapped = true
	}

	// Parse from string with inline error formatting
	file := parseSourceString(source, wrapped)

	// Merge standard library
	stdDir := findStdDir()
	if stdDir != "" {
		stdFiles := parseStdFiles(stdDir)
		file = mergeStdDecls(file, stdFiles)
	}

	// Semantic analysis
	info, errs := sema.Check(file)
	if len(errs) > 0 {
		printInlineErrors(source, errs, wrapped)
		os.Exit(1)
	}

	// Ownership analysis
	ownerErrs := ownership.Check(file, info)
	if len(ownerErrs) > 0 {
		printInlineErrors(source, ownerErrs, wrapped)
		os.Exit(1)
	}

	// Code generation
	result := codegen.Compile(file, info)

	// Compile and link to temp binary
	tmpOutput, err := os.CreateTemp("", "promise-exec-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating temp file: %v\n", err)
		os.Exit(1)
	}
	tmpOutput.Close()
	defer os.Remove(tmpOutput.Name())

	compileAndLink(result, tmpOutput.Name())

	// Execute with timeout
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, tmpOutput.Name())
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			fmt.Fprintf(os.Stderr, "\nTIMEOUT: execution exceeded %s timeout\n", timeout)
			os.Exit(1)
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "error running: %v\n", err)
		os.Exit(1)
	}
}

// isFullFile returns true if source looks like a complete Promise file
// with top-level declarations, false if it's just expressions/statements.
func isFullFile(source string) bool {
	if strings.Contains(source, "main(") {
		return true
	}
	trimmed := strings.TrimSpace(source)
	if strings.HasPrefix(trimmed, "type ") || strings.HasPrefix(trimmed, "enum ") ||
		strings.HasPrefix(trimmed, "use ") {
		return true
	}
	return false
}

// parseSourceString parses Promise source code from a string.
// Uses inline error formatting with source context display.
func parseSourceString(source string, wrapped bool) *ast.File {
	input := antlr.NewInputStream(source)

	el := &errorListener{source: source, wrapped: wrapped}

	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	lexer.AddErrorListener(el)

	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := parser.NewPromiseParser(stream)
	p.RemoveErrorListeners()
	p.AddErrorListener(el)

	tree := p.CompilationUnit()

	if el.errors > 0 {
		os.Exit(1)
	}

	file, errs := ast.Build("", tree)
	if len(errs) > 0 {
		printInlineErrors(source, errs, wrapped)
		os.Exit(1)
	}
	return file
}

// printInlineErrors formats errors with source context for inline execution.
func printInlineErrors(source string, errs []error, wrapped bool) {
	lines := strings.Split(source, "\n")
	lineOffset := 0
	if wrapped {
		lineOffset = 1
	}

	for _, e := range errs {
		var pos ast.Pos
		var msg string
		switch err := e.(type) {
		case *sema.Error:
			pos, msg = err.Pos, err.Msg
		case *ownership.Error:
			pos, msg = err.Pos, err.Msg
		default:
			fmt.Fprintln(os.Stderr, e)
			continue
		}

		displayLine := pos.Line - lineOffset
		fmt.Fprintf(os.Stderr, "%d:%d: %s\n", displayLine, pos.Column, msg)
		printErrorContext(lines, pos.Line-1, pos.Column)
	}
}

// printErrorContext prints source context around an error to stderr.
// It shows the previous line (when available) for context, then the
// error line with a caret marker. srcIdx is 0-based, column is 0-based.
func printErrorContext(lines []string, srcIdx, column int) {
	if srcIdx < 0 || srcIdx >= len(lines) {
		return
	}
	// Show the previous line for context when the error line is not the first.
	if srcIdx > 0 {
		fmt.Fprintf(os.Stderr, "    %s\n", lines[srcIdx-1])
	}
	fmt.Fprintf(os.Stderr, "  > %s\n", lines[srcIdx])
	if column >= 0 {
		fmt.Fprintf(os.Stderr, "    %s^\n", strings.Repeat(" ", column))
	}
}

// fileLineCache caches file contents read for error reporting.
var fileLineCache = map[string][]string{}

// readFileLines reads a file and returns its lines, caching results.
func readFileLines(filename string) []string {
	if lines, ok := fileLineCache[filename]; ok {
		return lines
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	fileLineCache[filename] = lines
	return lines
}

// printFileErrors formats errors with source context for file-based compilation.
func printFileErrors(filename string, errs []error) {
	lines := readFileLines(filename)

	for _, e := range errs {
		var pos ast.Pos
		var msg string
		switch err := e.(type) {
		case *sema.Error:
			pos, msg = err.Pos, err.Msg
		case *ownership.Error:
			pos, msg = err.Pos, err.Msg
		default:
			fmt.Fprintln(os.Stderr, e)
			continue
		}

		fmt.Fprintf(os.Stderr, "%s:%d:%d: %s\n", pos.File, pos.Line, pos.Column, msg)
		if lines != nil {
			printErrorContext(lines, pos.Line-1, pos.Column)
		}
	}
}

// --- Install ---

// runInstall installs the Promise compiler to ~/.promise/.
func runInstall() {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot determine home directory: %v\n", err)
		os.Exit(1)
	}

	promiseDir := filepath.Join(homeDir, ".promise")
	binDir := filepath.Join(promiseDir, "bin")
	libDir := filepath.Join(promiseDir, "lib")
	stdDest := filepath.Join(libDir, "std")

	// Create directory structure
	for _, dir := range []string{binDir, stdDest} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "error creating %s: %v\n", dir, err)
			os.Exit(1)
		}
	}

	// Copy binary
	execPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot determine executable path: %v\n", err)
		os.Exit(1)
	}
	copyFile(execPath, filepath.Join(binDir, "promise"), 0755)

	// Extract embedded std files
	extractEmbedded(embeddedStd, "resources/std", stdDest)

	// Extract embedded musl CRT (if available)
	if hasEmbeddedMuslCRT {
		arch := "x86_64-linux-musl"
		crtDest := filepath.Join(libDir, "crt", arch)
		if err := os.MkdirAll(crtDest, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "error creating %s: %v\n", crtDest, err)
			os.Exit(1)
		}
		extractEmbedded(embeddedMuslCRT, "resources/crt/"+arch, crtDest)
	}

	fmt.Printf("Installed Promise to %s\n\n", promiseDir)
	fmt.Printf("Add to your shell profile:\n\n")
	fmt.Printf("  export PATH=\"%s:$PATH\"\n", binDir)
}

// extractEmbedded writes all files from an embedded FS directory to a destination.
func extractEmbedded(fsys embed.FS, prefix, destDir string) {
	entries, err := fsys.ReadDir(prefix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading embedded %s: %v\n", prefix, err)
		os.Exit(1)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := fsys.ReadFile(prefix + "/" + e.Name())
		if err != nil {
			fmt.Fprintf(os.Stderr, "error reading embedded %s/%s: %v\n", prefix, e.Name(), err)
			os.Exit(1)
		}
		dst := filepath.Join(destDir, e.Name())
		if err := os.WriteFile(dst, data, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "error writing %s: %v\n", dst, err)
			os.Exit(1)
		}
	}
}

// copyFile copies a single file from src to dst with the given permissions.
func copyFile(src, dst string, perm os.FileMode) {
	data, err := os.ReadFile(src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading %s: %v\n", src, err)
		os.Exit(1)
	}
	if err := os.WriteFile(dst, data, perm); err != nil {
		fmt.Fprintf(os.Stderr, "error writing %s: %v\n", dst, err)
		os.Exit(1)
	}
}
