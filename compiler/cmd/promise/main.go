package main

import (
	"bytes"
	"compress/gzip"
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
	"sync"
	"time"

	"github.com/antlr4-go/antlr/v4"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/codegen"
	"djabi.dev/go/promise_lang/internal/module"
	"djabi.dev/go/promise_lang/internal/ownership"
	"djabi.dev/go/promise_lang/internal/parser"
	"djabi.dev/go/promise_lang/internal/sema"
	"djabi.dev/go/promise_lang/internal/types"
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
  doc       Generate documentation from doc() annotations
  ast       Print the AST
  exec      Execute inline Promise code
  init      Initialize a new Promise project (creates promise.toml)
  install   Install Promise to ~/.promise/

Options (build):
  -o <output>   Output file name (default: input file without extension)

Options (doc):
  -public         Show only public symbols (default)
  -all            Show all symbols including private
  -signatures     Compact mode: signatures only, no doc text
  -o <output>     Write output to file instead of stdout

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
	case "doc":
		runDoc(os.Args[2:])
	case "init":
		runInit()
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
	filename, outputFile, _ := buildToFile(args)
	fmt.Printf("Compiled %s → %s\n", filename, outputFile)
}

// buildToFile compiles a .pr file to an executable, returning the source path,
// output path, and target triple.
func buildToFile(args []string) (filename, outputFile, target string) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-o":
			if i+1 < len(args) {
				outputFile = args[i+1]
				i++
			}
		case "-target", "--target":
			if i+1 < len(args) {
				target = args[i+1]
				i++
			}
		default:
			filename = args[i]
		}
	}

	if filename == "" {
		fmt.Fprintln(os.Stderr, "usage: promise build [-o output] [--target triple] <file.pr>")
		os.Exit(1)
	}

	if target == "" {
		target = codegen.HostTargetTriple()
	}

	if outputFile == "" {
		base := strings.TrimSuffix(filepath.Base(filename), ".pr")
		if isWasmTarget(target) {
			outputFile = base + ".wasm"
		} else {
			outputFile = base
		}
	}

	file, info := compileFrontend(filename)
	result := codegen.Compile(file, info, target)

	compileAndLink(result, outputFile, target)
	return filename, outputFile, target
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
	var targetTriple string          // empty = host target
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
		} else if (args[i] == "-target" || args[i] == "--target") && i+1 < len(args) {
			targetTriple = args[i+1]
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

	// Print target when cross-compiling
	if targetTriple != "" && targetTriple != codegen.HostTargetTriple() {
		fmt.Printf("target: %s\n", targetTriple)
	}

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
		runStress(files, stressCount, stressDuration, timeout, targetTriple)
		return
	}

	if info.IsDir() {
		runTestDir(target, recursive, timeout, targetTriple)
	} else {
		runTestFile(target, timeout, targetTriple)
	}
}

// runTestFile runs test functions from a single .pr file.
// targetTriple overrides the compilation target (empty = host).
func runTestFile(filename string, timeout time.Duration, targetTriple string) {
	start := time.Now()

	// Frontend compilation (parse + merge std + sema + ownership)
	file, info := compileFrontend(filename)

	if info.HasExpectOutput {
		runE2ETest(file, info, filename, timeout, start, targetTriple)
		return
	}

	if len(info.Tests) == 0 {
		fmt.Println("no tests found")
		return
	}

	// Codegen
	target := targetTriple
	if target == "" {
		target = codegen.HostTargetTriple()
	}
	result := codegen.Compile(file, info, target)

	// Generate test main (replaces user main)
	result.GenerateTestMain(info.Tests)

	// Link to temp binary (test runner is now codegen-emitted, no C files needed)
	ext := ""
	if isWasmTarget(target) {
		ext = ".wasm"
	}
	tmpOutput, err := os.CreateTemp("", "promise-test-*"+ext)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating temp file: %v\n", err)
		os.Exit(1)
	}
	tmpOutput.Close()
	defer os.Remove(tmpOutput.Name())

	compileAndLink(result, tmpOutput.Name(), target)

	// Execute test binary with timeout
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var cmd *exec.Cmd
	if isWasmTarget(target) {
		cmd = exec.CommandContext(ctx, "wasmtime", tmpOutput.Name())
	} else {
		cmd = exec.CommandContext(ctx, tmpOutput.Name())
	}
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
func runE2ETest(file *ast.File, info *sema.Info, filename string, timeout time.Duration, start time.Time, targetTriple string) {
	name := strings.TrimSuffix(filepath.Base(filename), ".pr")

	// Resolve target
	target := targetTriple
	if target == "" {
		target = codegen.HostTargetTriple()
	}

	// Check target exclusion
	if isTestExcluded(target, info.ExcludeTargets) {
		fmt.Printf("SKIP (excluded) %s\n", name)
		return
	}

	// Codegen with normal main (no GenerateTestMain)
	result := codegen.Compile(file, info, target)

	ext := ""
	if isWasmTarget(target) {
		ext = ".wasm"
	}
	tmpOutput, err := os.CreateTemp("", "promise-e2e-*"+ext)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating temp file: %v\n", err)
		os.Exit(1)
	}
	tmpOutput.Close()
	defer os.Remove(tmpOutput.Name())

	compileAndLink(result, tmpOutput.Name(), target)

	// Execute with timeout, capturing combined stdout+stderr
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var cmd *exec.Cmd
	if isWasmTarget(target) {
		cmd = exec.CommandContext(ctx, "wasmtime", tmpOutput.Name())
	} else {
		cmd = exec.CommandContext(ctx, tmpOutput.Name())
	}
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
		fmt.Printf("PASS (%.3fs)\n", elapsed.Seconds())
		fmt.Printf("1 passed, 0 failed (%.3fs)\n", elapsed.Seconds())
	} else {
		fmt.Printf("FAIL (%.3fs)\n", elapsed.Seconds())
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
func runTestDir(dir string, recursive bool, timeout time.Duration, targetTriple string) {
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
	failLineRe := regexp.MustCompile(`^FAIL \(\d+\.\d+s\)(?: (.+))?$`)

	totalPassed := 0
	totalFailed := 0
	totalFiles := 0
	failedFiles := 0
	var failures []string

	for _, f := range files {
		// Run "promise test <file>" as subprocess with timeout
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		testArgs := []string{"test", "-timeout", fmt.Sprintf("%gs", timeout.Seconds())}
		if targetTriple != "" {
			testArgs = append(testArgs, "--target", targetTriple)
		}
		testArgs = append(testArgs, f)
		cmd := exec.CommandContext(ctx, selfExe, testArgs...)
		output, err := cmd.CombinedOutput()
		timedOut := ctx.Err() == context.DeadlineExceeded
		cancel()
		outStr := strings.TrimSpace(string(output))

		// Skip files with no tests or excluded for this target
		if !timedOut && (outStr == "no tests found" || strings.HasPrefix(outStr, "SKIP")) {
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
					if m[1] != "" {
						failures = append(failures, relPath+": "+m[1])
					} else {
						failures = append(failures, relPath)
					}
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
			// Skip module directories (contain promise.toml) — their .pr files
			// are module source, not test files, and can't be compiled standalone.
			if d.IsDir() && path != dir {
				if _, err := os.Stat(filepath.Join(path, "promise.toml")); err == nil {
					return filepath.SkipDir
				}
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
// On Linux, macOS, and WASM, uses opt + llc + linker pipeline (Phase 7a/7b/7c).
// On other platforms (or with PROMISE_USE_CLANG=1), uses clang as driver.
func compileAndLink(result *codegen.CompileResult, outputFile, target string) {
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
	// Linux, macOS, and WASM use the LLVM pipeline. Other platforms use clang.
	return !strings.Contains(target, "linux") && !strings.Contains(target, "macosx") && !strings.Contains(target, "wasm")
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

// --- Embedded LLVM tool extraction ---

// llvmExtractOnce ensures embedded LLVM tools are extracted at most once per process.
var llvmExtractOnce sync.Once

// llvmCacheDir is set by ensureEmbeddedLLVM after successful extraction.
var llvmCacheDir string

// embeddedLLVMFiles are the compressed files we expect in the embed FS.
// The base names (without .gz) become executables in the cache dir.
var embeddedLLVMFiles = []string{"opt.gz", "llc.gz", "lld.gz", "libLLVM.so.gz"}

// embeddedLLVMSymlinks maps symlink name → target for lld mode selection.
var embeddedLLVMSymlinks = map[string]string{
	"ld.lld":   "lld",
	"ld64.lld": "lld",
	"lld-link": "lld",
	"wasm-ld":  "lld",
}

// llvmCacheDirPath returns the path where embedded LLVM tools are extracted.
func llvmCacheDirPath() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(homeDir, ".promise", "cache", "llvm", "linux-amd64"), nil
}

// llvmCacheComplete checks if all expected LLVM tools exist in the cache dir.
func llvmCacheComplete(dir string) bool {
	for _, gz := range embeddedLLVMFiles {
		name := strings.TrimSuffix(gz, ".gz")
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return false
		}
	}
	// Also check symlinks
	for link := range embeddedLLVMSymlinks {
		if _, err := os.Lstat(filepath.Join(dir, link)); err != nil {
			return false
		}
	}
	return true
}

// ensureEmbeddedLLVM extracts compressed LLVM tools from the embedded FS to the cache dir.
// Called at most once per process via llvmExtractOnce.
func ensureEmbeddedLLVM() {
	if !hasEmbeddedLLVM {
		return
	}

	dir, err := llvmCacheDirPath()
	if err != nil {
		return
	}

	// Check if cache is already complete
	if llvmCacheComplete(dir) {
		llvmCacheDir = dir
		return
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot create LLVM cache dir %s: %v\n", dir, err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "Extracting embedded LLVM tools to %s...\n", dir)
	extractCompressedLLVM(dir)
	llvmCacheDir = dir
}

// extractCompressedLLVM decompresses embedded LLVM tools to the given directory.
// Used by both ensureEmbeddedLLVM (cache) and runInstall (install dir).
func extractCompressedLLVM(destDir string) {
	prefix := "resources/llvm/linux-amd64"
	for _, gz := range embeddedLLVMFiles {
		data, err := embeddedLLVM.ReadFile(prefix + "/" + gz)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: cannot read embedded %s: %v\n", gz, err)
			os.Exit(1)
		}

		gr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: cannot decompress %s: %v\n", gz, err)
			os.Exit(1)
		}

		name := strings.TrimSuffix(gz, ".gz")
		outPath := filepath.Join(destDir, name)
		out, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		if err != nil {
			gr.Close()
			fmt.Fprintf(os.Stderr, "error: cannot write %s: %v\n", outPath, err)
			os.Exit(1)
		}

		if _, err := io.Copy(out, gr); err != nil {
			out.Close()
			gr.Close()
			fmt.Fprintf(os.Stderr, "error: cannot decompress %s: %v\n", gz, err)
			os.Exit(1)
		}
		out.Close()
		gr.Close()
	}

	// Create symlinks for lld modes
	for link, target := range embeddedLLVMSymlinks {
		linkPath := filepath.Join(destDir, link)
		os.Remove(linkPath)
		if err := os.Symlink(target, linkPath); err != nil {
			fmt.Fprintf(os.Stderr, "error: cannot create symlink %s → %s: %v\n", link, target, err)
			os.Exit(1)
		}
	}
}

// --- LLVM tool pipeline ---

// findLLVMTool locates an LLVM tool (opt, llc, ld.lld, ld64.lld) by searching:
// 1. Sibling directory of the promise binary
// 2. Environment variable override (PROMISE_OPT, PROMISE_LLC, PROMISE_LLD, PROMISE_LD64LLD)
// 3. Embedded LLVM cache (~/.promise/cache/llvm/linux-amd64/)
// 4. Homebrew LLVM (macOS)
// 5. Versioned names on PATH (e.g., opt-22, llc-22, ld.lld-22) from newest to minLLVMMajor
// 6. Unversioned names on PATH (e.g., opt, llc, ld.lld)
func findLLVMTool(name string) (string, error) {
	envMap := map[string]string{
		"opt":      "PROMISE_OPT",
		"llc":      "PROMISE_LLC",
		"ld.lld":   "PROMISE_LLD",
		"ld64.lld": "PROMISE_LD64LLD",
		"wasm-ld":  "PROMISE_WASM_LD",
	}

	// 1. Sibling of promise binary (also check llvm/ subdirectory for install layout)
	if execPath, err := os.Executable(); err == nil {
		dir := filepath.Dir(execPath)
		sibling := filepath.Join(dir, name)
		if _, err := os.Stat(sibling); err == nil {
			return sibling, nil
		}
		subdir := filepath.Join(dir, "llvm", name)
		if _, err := os.Stat(subdir); err == nil {
			return subdir, nil
		}
	}

	// 2. Env override
	if envName, ok := envMap[name]; ok {
		if p := os.Getenv(envName); p != "" {
			return p, nil
		}
	}

	// 3. Embedded LLVM cache (Linux only — extract on first access)
	if hasEmbeddedLLVM {
		llvmExtractOnce.Do(ensureEmbeddedLLVM)
		if llvmCacheDir != "" {
			p := filepath.Join(llvmCacheDir, name)
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		}
	}

	// 4. Homebrew LLVM/LLD (macOS only)
	if runtime.GOOS == "darwin" {
		for _, prefix := range []string{
			"/opt/homebrew/opt/llvm/bin",
			"/usr/local/opt/llvm/bin",
			"/opt/homebrew/opt/lld/bin",
			"/usr/local/opt/lld/bin",
		} {
			p := filepath.Join(prefix, name)
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		}
	}

	// 5. Versioned names on PATH (try newest to oldest)
	for v := maxLLVMSearch; v >= minLLVMMajor; v-- {
		versioned := fmt.Sprintf("%s-%d", name, v)
		if path, err := exec.LookPath(versioned); err == nil {
			return path, nil
		}
	}

	// 6. Unversioned on PATH
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}

	envName := envMap[name]
	return "", fmt.Errorf("%s not found\n  searched: sibling of promise binary, $%s, embedded cache, Homebrew LLVM, PATH (%s-{%d..%d}, %s)\n  install LLVM %d+ or set PROMISE_USE_CLANG=1 to use clang",
		name, envName, name, maxLLVMSearch, minLLVMMajor, name, minLLVMMajor)
}

// runLLVMCmd creates an exec.Cmd for an LLVM tool, setting LD_LIBRARY_PATH
// so dynamically-linked tools can find libLLVM.so when running from the cache dir.
func runLLVMCmd(toolPath string, args ...string) *exec.Cmd {
	cmd := exec.Command(toolPath, args...)
	// If the tool is in the embedded cache, ensure LD_LIBRARY_PATH includes that dir
	// so it can find libLLVM.so alongside it.
	toolDir := filepath.Dir(toolPath)
	if llvmCacheDir != "" && toolDir == llvmCacheDir {
		env := os.Environ()
		ldPath := os.Getenv("LD_LIBRARY_PATH")
		if ldPath != "" {
			ldPath = toolDir + ":" + ldPath
		} else {
			ldPath = toolDir
		}
		// Replace or append LD_LIBRARY_PATH
		found := false
		for i, e := range env {
			if strings.HasPrefix(e, "LD_LIBRARY_PATH=") {
				env[i] = "LD_LIBRARY_PATH=" + ldPath
				found = true
				break
			}
		}
		if !found {
			env = append(env, "LD_LIBRARY_PATH="+ldPath)
		}
		cmd.Env = env
	}
	return cmd
}

// llvmToolVersion returns the major version of an LLVM tool, or 0 if it cannot be determined.
// Handles different version formats:
//   - opt/llc: "LLVM version 22.1.2"
//   - ld.lld:  "LLD 22.1.2" (no "LLVM version" prefix)
func llvmToolVersion(toolPath string) int {
	cmd := runLLVMCmd(toolPath, "--version")
	out, err := cmd.Output()
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

// isWasmTarget returns true if the target triple is WebAssembly.
func isWasmTarget(target string) bool {
	return strings.Contains(target, "wasm")
}

// isTestExcluded checks if the current target matches any of the exclude substrings.
func isTestExcluded(target string, excludes []string) bool {
	for _, ex := range excludes {
		if strings.Contains(target, ex) {
			return true
		}
	}
	return false
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
// Linux: opt → llc → ld.lld. macOS: opt → llc → ld64.lld (or system ld). WASM: opt → llc → wasm-ld.
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

	optCmd := runLLVMCmd(optPath, "-O1", llFile, "-o", bcFile.Name())
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

	llcArgs := []string{
		"-mtriple=" + target,
		"-filetype=obj",
	}
	if isWasmTarget(target) {
		llcArgs = append(llcArgs, "-mattr=+bulk-memory,+mutable-globals,+sign-ext")
	} else {
		llcArgs = append(llcArgs, "-relocation-model=pic")
	}
	llcArgs = append(llcArgs, bcFile.Name(), "-o", objFile.Name())

	llcCmd := exec.Command(llcPath, llcArgs...)
	llcCmd.Stderr = os.Stderr
	if err := llcCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error running llc: %v\n", err)
		os.Exit(1)
	}

	// Step 3: Link (platform-specific)
	if isDarwinTarget(target) {
		linkDarwin(objFile.Name(), target, outputFile)
	} else if isWasmTarget(target) {
		linkWasm(objFile.Name(), outputFile)
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
	var linkCmd *exec.Cmd
	if isLLD {
		linkCmd = runLLVMCmd(linkerPath, linkArgs...)
	} else {
		linkCmd = exec.Command(linkerPath, linkArgs...)
	}
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

// linkWasm runs wasm-ld for WebAssembly linking.
func linkWasm(objFile, outputFile string) {
	lldPath, err := findLLVMTool("wasm-ld")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	checkLLVMToolVersion(lldPath)

	linkArgs := buildWasmLinkArgs(objFile, outputFile)
	linkCmd := exec.Command(lldPath, linkArgs...)
	linkCmd.Stderr = os.Stderr
	if err := linkCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error linking (wasm-ld): %v\n", err)
		os.Exit(1)
	}
}

// ensureWasmAllocObj extracts the embedded WASM allocator object to cache.
// Returns the path to the .o file.
func ensureWasmAllocObj() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home dir: %v", err)
	}
	cacheDir := filepath.Join(homeDir, ".promise", "cache", "crt", "wasm32")
	objPath := filepath.Join(cacheDir, "wasm_alloc.o")

	// Check if cached version matches embedded (by size)
	if info, err := os.Stat(objPath); err == nil {
		if info.Size() == int64(len(embeddedWasmAllocObj)) {
			return objPath, nil
		}
	}

	// Extract to cache
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", fmt.Errorf("cannot create WASM CRT cache: %v", err)
	}
	if err := os.WriteFile(objPath, embeddedWasmAllocObj, 0644); err != nil {
		return "", fmt.Errorf("cannot write wasm_alloc.o to cache: %v", err)
	}
	return objPath, nil
}

// buildWasmLinkArgs builds the wasm-ld argument list for WASI linking.
// Links user code with the embedded free-list allocator (wasm_alloc.o).
func buildWasmLinkArgs(objFile, outputFile string) []string {
	allocObj, err := ensureWasmAllocObj()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	return []string{
		"--no-entry",
		"--export=_start",
		"--allow-undefined", // WASI imports (fd_write, proc_exit) resolved at runtime
		"-o", outputFile,
		objFile,
		allocObj,
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
	linkCmd := runLLVMCmd(lldPath, linkArgs...)
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
	var stdFiles []*ast.File
	if stdDir != "" {
		stdFiles = parseStdFiles(stdDir)
		file = mergeStdDecls(file, stdFiles)
	}

	// Load local modules from use declarations
	moduleScopes, modInfos := loadModuleScopes(filename, file, stdFiles)

	info, errs := sema.CheckWithModules(file, moduleScopes)
	if modInfos != nil {
		info.ModuleInfos = modInfos
	}
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

// loadModuleScopes scans use declarations for local module paths, loads each
// module (parse + sema), and returns scopes for sema + ModuleInfos for codegen.
func loadModuleScopes(filename string, file *ast.File, stdFiles []*ast.File) (map[string]*types.Scope, map[string]*sema.ModuleInfo) {
	if len(file.Uses) == 0 {
		return nil, nil
	}

	// Find project root (directory containing promise.toml).
	// Fall back to the source file's directory for single-file mode.
	projectRoot := filepath.Dir(filename)
	if abs, err := filepath.Abs(projectRoot); err == nil {
		projectRoot = abs
	}
	if cfg, err := module.FindConfig(projectRoot); err == nil && cfg != nil {
		projectRoot = cfg.Dir
	}

	scopes := make(map[string]*types.Scope)
	modInfos := make(map[string]*sema.ModuleInfo)
	for _, u := range file.Uses {
		if u.Path == "" || !module.IsLocalPath(u.Path) {
			continue // catalog or remote — skip for now
		}

		modInfo, err := loadLocalModule(u.Path, projectRoot, stdFiles)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: error loading module '%s': %v\n", filename, u.Path, err)
			os.Exit(1)
		}
		// Use the alias from the use declaration as the module name for codegen.
		// This ensures qualified calls like vis.func() resolve correctly even when
		// the alias differs from the directory name (e.g., use vis "./visibility").
		if u.Alias != "_" {
			modInfo.Name = u.Alias
		}
		scopes[u.Path] = sema.ExportedScope(modInfo.SemaInfo, modInfo.File)
		modInfos[u.Path] = modInfo
	}

	if len(scopes) == 0 {
		return nil, nil
	}
	return scopes, modInfos
}

// loadLocalModule parses all .pr files in the module directory, runs sema,
// and returns a ModuleInfo containing the AST, sema output, and exported scope.
func loadLocalModule(modPath, projectRoot string, stdFiles []*ast.File) (*sema.ModuleInfo, error) {
	// Resolve module directory (paths are relative to project root)
	modDir := filepath.Join(projectRoot, modPath)
	absDir, err := filepath.Abs(modDir)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve path: %w", err)
	}

	info, err := os.Stat(absDir)
	if err != nil {
		return nil, fmt.Errorf("module directory not found: %s", absDir)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("not a directory: %s", absDir)
	}

	// Check for promise.toml
	tomlPath := filepath.Join(absDir, "promise.toml")
	if _, err := os.Stat(tomlPath); err != nil {
		return nil, fmt.Errorf("module directory '%s' has no promise.toml", absDir)
	}

	// Parse all .pr files in the module directory
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return nil, fmt.Errorf("cannot read module directory: %w", err)
	}

	var modFileList []*ast.File
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".pr") {
			f := parseSourceFile(filepath.Join(absDir, e.Name()))
			modFileList = append(modFileList, f)
		}
	}

	if len(modFileList) == 0 {
		return nil, fmt.Errorf("module '%s' contains no .pr files", modPath)
	}

	// Merge module files into a single AST, then merge std decls
	merged := mergeModuleFiles(modFileList)
	if len(stdFiles) > 0 {
		merged = mergeStdDecls(merged, stdFiles)
	}

	// Run sema on the module
	semaInfo, errs := sema.Check(merged)
	if len(errs) > 0 {
		return nil, fmt.Errorf("errors in module '%s': %v", modPath, errs[0])
	}

	// Extract the module alias from the path (last component)
	alias := filepath.Base(modPath)

	return &sema.ModuleInfo{
		Name:     alias,
		Path:     modPath,
		File:     merged,
		SemaInfo: semaInfo,
	}, nil
}

// mergeModuleFiles combines multiple parsed .pr files from a module directory
// into a single AST file. Use declarations and top-level declarations are merged.
func mergeModuleFiles(files []*ast.File) *ast.File {
	if len(files) == 1 {
		return files[0]
	}
	merged := files[0]
	for _, f := range files[1:] {
		merged.Uses = append(merged.Uses, f.Uses...)
		merged.Decls = append(merged.Decls, f.Decls...)
	}
	return merged
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
	target := ""
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
		} else if (args[i] == "-target" || args[i] == "--target") && i+1 < len(args) {
			target = args[i+1]
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
	if target == "" {
		target = codegen.HostTargetTriple()
	}
	result := codegen.Compile(file, info, target)

	// Compile and link to temp binary
	ext := ""
	if isWasmTarget(target) {
		ext = ".wasm"
	}
	tmpOutput, err := os.CreateTemp("", "promise-exec-*"+ext)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating temp file: %v\n", err)
		os.Exit(1)
	}
	tmpOutput.Close()
	defer os.Remove(tmpOutput.Name())

	compileAndLink(result, tmpOutput.Name(), target)

	// Execute with timeout
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var cmd *exec.Cmd
	if isWasmTarget(target) {
		cmd = exec.CommandContext(ctx, "wasmtime", tmpOutput.Name())
	} else {
		cmd = exec.CommandContext(ctx, tmpOutput.Name())
	}
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
// runInit creates a promise.toml in the current directory.
func runInit() {
	const defaultEpoch = "2026.3"

	if _, err := os.Stat("promise.toml"); err == nil {
		fmt.Fprintln(os.Stderr, "promise.toml already exists")
		os.Exit(1)
	}

	// Use directory name as default module name
	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	name := filepath.Base(dir)

	content := fmt.Sprintf("[module]\nname = %q\nepoch = %q\n", name, defaultEpoch)
	if err := os.WriteFile("promise.toml", []byte(content), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "error writing promise.toml: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Created promise.toml (module: %s, epoch: %s)\n", name, defaultEpoch)
}

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

	// Extract embedded LLVM tools (if available)
	if hasEmbeddedLLVM {
		llvmDest := filepath.Join(binDir, "llvm")
		if err := os.MkdirAll(llvmDest, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "error creating %s: %v\n", llvmDest, err)
			os.Exit(1)
		}
		extractCompressedLLVM(llvmDest)
	}

	fmt.Printf("Installed Promise to %s\n", promiseDir)
	fmt.Printf("  binary: %s\n", filepath.Join(binDir, "promise"))
	fmt.Printf("  std:    %s\n", stdDest)
	if hasEmbeddedLLVM {
		fmt.Printf("  llvm:   %s\n", filepath.Join(binDir, "llvm"))
	}
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
