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

	if len(remaining) < 1 {
		fmt.Fprintln(os.Stderr, "usage: promise test [-timeout duration] <file.pr | dir | dir/...>")
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

	if info.IsDir() {
		runTestDir(target, recursive, timeout)
	} else {
		runTestFile(target, timeout)
	}
}

// runTestFile runs test functions from a single .pr file.
func runTestFile(filename string, timeout time.Duration) {
	// Frontend compilation (parse + merge std + sema + ownership)
	file, info := compileFrontend(filename)

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
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			fmt.Fprintf(os.Stderr, "\nTIMEOUT: tests exceeded %s timeout\n", timeout)
			os.Exit(1)
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "error running tests: %v\n", err)
		os.Exit(1)
	}
}

// runTestDir discovers .pr files in a directory and runs tests from each.
func runTestDir(dir string, recursive bool, timeout time.Duration) {
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

	summaryRe := regexp.MustCompile(`^(\d+) passed, (\d+) failed$`)

	totalPassed := 0
	totalFailed := 0
	totalFiles := 0
	failedFiles := 0

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
			fmt.Printf("TIMEOUT: exceeded %s timeout\n\n", timeout)
			failedFiles++
			totalFailed++
			continue
		}

		if err != nil {
			// Compilation or runtime error
			fmt.Println(outStr)
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
			continue
		}

		// Parse summary before printing
		if m := summaryRe.FindStringSubmatch(lastLine(outStr)); m != nil {
			totalPassed += atoi(m[1])
			totalFailed += atoi(m[2])
		}

		// Print test output (strip the summary line — we'll print our own)
		lines := strings.Split(outStr, "\n")
		for _, line := range lines {
			if summaryRe.MatchString(line) {
				continue
			}
			fmt.Println(line)
		}
		fmt.Println()
	}

	if totalFiles == 0 {
		fmt.Println("no test files found")
		return
	}

	// Print grand summary
	fmt.Printf("%d passed, %d failed (%d files)\n", totalPassed, totalFailed, totalFiles)
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

	// All runtime functions are codegen-emitted LLVM IR — no C files needed.
	// -O1 ensures LLVM coroutine passes (CoroSplit, CoroElide) run for M:N scheduler goroutines.
	linkArgs := []string{"-O1", "-target", target, llFile.Name(), "-o", outputFile}
	// Linux requires explicit -lpthread for PAL threading primitives.
	// macOS includes pthreads in libSystem (already linked).
	if strings.Contains(target, "linux") {
		linkArgs = append(linkArgs, "-lpthread")
	}
	linkCmd := exec.Command(findClang(), linkArgs...)
	linkCmd.Stderr = os.Stderr
	if err := linkCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error linking: %v\n", err)
		os.Exit(1)
	}
}

// findClang returns the path to a clang binary.
// Prefers Homebrew LLVM (needed for coroutine intrinsics) over Apple clang.
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
