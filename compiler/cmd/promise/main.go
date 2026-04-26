package main

import (
	"fmt"
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

func usage() {
	fmt.Fprintf(os.Stderr, `Usage: promise <command> [options] <file.pr>

Commands:
  build   Compile a Promise source file to an executable
  run     Compile and run a Promise source file
  check   Run semantic analysis (type checking)
  ast     Print the AST

Options (build):
  -o <output>   Output file name (default: input file without extension)
`)
}

func main() {
	if len(os.Args) < 2 {
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

	// Write .ll to temp file
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

	// Generate C header for runtime type verification
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

	// Find runtime directory relative to the binary or in known locations.
	// Also update the reference header for IDE support (types only, no externs).
	runtimeDir := findRuntimeDir()
	if runtimeDir == "" {
		fmt.Fprintln(os.Stderr, "error: cannot find runtime/ directory")
		os.Exit(1)
	}

	// Best-effort: refresh runtime/promise_bindings.h with current type layouts
	if refFile, err := os.Create(filepath.Join(runtimeDir, "promise_bindings.h")); err == nil {
		_ = codegen.GenerateHeader(refFile, result.Layouts, result.EnumLayouts, nil)
		refFile.Close()
	}

	// Compile all .c files in the runtime directory with generated header
	runtimeCFiles, err := findRuntimeCFiles(runtimeDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading runtime directory: %v\n", err)
		os.Exit(1)
	}

	var runtimeObjs []string
	for _, cFile := range runtimeCFiles {
		objFile, err := os.CreateTemp("", "promise-runtime-*.o")
		if err != nil {
			fmt.Fprintf(os.Stderr, "error creating temp file: %v\n", err)
			os.Exit(1)
		}
		objFile.Close()
		defer os.Remove(objFile.Name())

		clangCmd := exec.Command("clang", "-c", cFile, "-include", headerFile.Name(), "-o", objFile.Name())
		clangCmd.Stderr = os.Stderr
		if err := clangCmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "error compiling %s: %v\n", filepath.Base(cFile), err)
			os.Exit(1)
		}
		runtimeObjs = append(runtimeObjs, objFile.Name())
	}

	// Link
	linkArgs := []string{llFile.Name()}
	linkArgs = append(linkArgs, runtimeObjs...)
	linkArgs = append(linkArgs, "-o", outputFile)
	linkCmd := exec.Command("clang", linkArgs...)
	linkCmd.Stderr = os.Stderr
	if err := linkCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error linking: %v\n", err)
		os.Exit(1)
	}

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

// compileFrontend runs the full frontend pipeline: parse → sema → ownership.
func compileFrontend(filename string) (*ast.File, *sema.Info) {
	file := parseSourceFile(filename)

	info, errs := sema.Check(file)
	if len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintln(os.Stderr, e)
		}
		os.Exit(1)
	}

	ownerErrs := ownership.Check(file, info)
	if len(ownerErrs) > 0 {
		for _, e := range ownerErrs {
			fmt.Fprintln(os.Stderr, e)
		}
		os.Exit(1)
	}

	return file, info
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

// findRuntimeCFiles returns all .c files in the runtime directory, sorted.
func findRuntimeCFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".c") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	return files, nil
}

type errorListener struct {
	antlr.DefaultErrorListener
	filename string
	errors   int
}

func (l *errorListener) SyntaxError(
	_ antlr.Recognizer,
	_ interface{},
	line, column int,
	msg string,
	_ antlr.RecognitionException,
) {
	fmt.Fprintf(os.Stderr, "%s:%d:%d: %s\n", l.filename, line, column, msg)
	l.errors++
}
