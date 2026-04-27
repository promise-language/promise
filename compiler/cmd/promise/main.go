package main

import (
	"embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/antlr4-go/antlr/v4"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/codegen"
	"djabi.dev/go/promise_lang/internal/ownership"
	"djabi.dev/go/promise_lang/internal/parser"
	"djabi.dev/go/promise_lang/internal/sema"
)

//go:embed resources/std/*.pr
var embeddedStd embed.FS

//go:embed resources/runtime/*
var embeddedRuntime embed.FS

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

Inline execution:
  promise exec 'println("hello")'
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
	var outputFile string
	var filename string

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

	compileAndLink(result, outputFile, false)
	fmt.Printf("Compiled %s → %s\n", filename, outputFile)
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
	runBuild(buildArgs)

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

// runTest discovers and runs `test annotated functions in a .pr file.
func runTest(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: promise test <file.pr>")
		os.Exit(1)
	}

	filename := args[0]

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

	// Link to temp binary (include runtime_test.c for fork isolation)
	tmpOutput, err := os.CreateTemp("", "promise-test-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating temp file: %v\n", err)
		os.Exit(1)
	}
	tmpOutput.Close()
	defer os.Remove(tmpOutput.Name())

	compileAndLink(result, tmpOutput.Name(), true)

	// Execute test binary
	cmd := exec.Command(tmpOutput.Name())
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "error running tests: %v\n", err)
		os.Exit(1)
	}
}

// compileAndLink writes the IR to a temp file, compiles the C runtime,
// and links everything into the output binary.
func compileAndLink(result *codegen.CompileResult, outputFile string, testMode bool) {
	llFile, err := os.CreateTemp("", "promise-*.ll")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating temp file: %v\n", err)
		os.Exit(1)
	}
	defer os.Remove(llFile.Name())

	if _, err := fmt.Fprint(llFile, result.Module.String()); err != nil {
		fmt.Fprintf(os.Stderr, "error writing IR: %v\n", err)
		os.Exit(1)
	}
	llFile.Close()

	headerFile, err := os.CreateTemp("", "promise_bindings-*.h")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating header file: %v\n", err)
		os.Exit(1)
	}
	defer os.Remove(headerFile.Name())

	if err := codegen.GenerateHeader(headerFile, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		fmt.Fprintf(os.Stderr, "error generating header: %v\n", err)
		os.Exit(1)
	}
	headerFile.Close()

	runtimeDir := findRuntimeDir()
	if runtimeDir == "" {
		fmt.Fprintln(os.Stderr, "error: cannot find runtime/ directory")
		os.Exit(1)
	}

	runtimeCFiles, err := findRuntimeCFiles(runtimeDir, testMode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading runtime directory: %v\n", err)
		os.Exit(1)
	}

	target := codegen.HostTargetTriple()
	var runtimeObjs []string
	for _, cFile := range runtimeCFiles {
		objFile, err := os.CreateTemp("", "promise-runtime-*.o")
		if err != nil {
			fmt.Fprintf(os.Stderr, "error creating temp file: %v\n", err)
			os.Exit(1)
		}
		objFile.Close()
		defer os.Remove(objFile.Name())

		clangCmd := exec.Command("clang", "-target", target, "-c", cFile, "-include", headerFile.Name(), "-o", objFile.Name())
		clangCmd.Stderr = os.Stderr
		if err := clangCmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "error compiling %s: %v\n", filepath.Base(cFile), err)
			os.Exit(1)
		}
		runtimeObjs = append(runtimeObjs, objFile.Name())
	}

	linkArgs := []string{"-target", target, llFile.Name()}
	linkArgs = append(linkArgs, runtimeObjs...)
	linkArgs = append(linkArgs, "-o", outputFile)
	linkCmd := exec.Command("clang", linkArgs...)
	linkCmd.Stderr = os.Stderr
	if err := linkCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error linking: %v\n", err)
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

// findRuntimeDir searches for the runtime/ directory in standard locations.
func findRuntimeDir() string {
	candidates := []string{
		"runtime",
		"../runtime",
		"../../runtime",
	}

	// Check relative to executable
	if execPath, err := os.Executable(); err == nil {
		dir := filepath.Dir(execPath)
		candidates = append(candidates,
			filepath.Join(dir, "runtime"),
			filepath.Join(dir, "..", "runtime"),
			filepath.Join(dir, "..", "..", "runtime"),
		)
	}

	// Check installed location
	if homeDir, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(homeDir, ".promise", "lib", "runtime"),
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

// findRuntimeCFiles returns .c files in the runtime directory.
// If testMode is true, includes runtime_test.c; otherwise excludes it.
func findRuntimeCFiles(dir string, testMode bool) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".c") {
			if !testMode && e.Name() == "runtime_test.c" {
				continue
			}
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	return files, nil
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
		srcIdx := line - 1
		if srcIdx >= 0 && srcIdx < len(lines) {
			fmt.Fprintf(os.Stderr, "    %s\n", lines[srcIdx])
			fmt.Fprintf(os.Stderr, "    %s^\n", strings.Repeat(" ", column))
		}
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
	var source string

	if len(args) > 0 {
		source = strings.Join(args, " ")
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

	compileAndLink(result, tmpOutput.Name(), false)

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

// printErrorContext prints a source line and caret marker to stderr.
// srcIdx is the 0-based line index, column is 0-based.
func printErrorContext(lines []string, srcIdx, column int) {
	if srcIdx >= 0 && srcIdx < len(lines) {
		fmt.Fprintf(os.Stderr, "    %s\n", lines[srcIdx])
		if column >= 0 {
			fmt.Fprintf(os.Stderr, "    %s^\n", strings.Repeat(" ", column))
		}
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
	runtimeDest := filepath.Join(libDir, "runtime")

	// Create directory structure
	for _, dir := range []string{binDir, stdDest, runtimeDest} {
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

	// Extract embedded runtime files
	extractEmbedded(embeddedRuntime, "resources/runtime", runtimeDest)

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
