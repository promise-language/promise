package main

import (
	"context"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
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

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/ast/astcache"
	"github.com/promise-language/promise/compiler/internal/blobstore"
	"github.com/promise-language/promise/compiler/internal/codegen"
	"github.com/promise-language/promise/compiler/internal/module"
	"github.com/promise-language/promise/compiler/internal/ownership"
	"github.com/promise-language/promise/compiler/internal/parser"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
)

// normalizeArgs canonicalizes CLI flag arguments so that both -/-- prefixes
// resolve identically, and =/: value separators are accepted alongside spaces.
// See NormalizeArgs in tools/build/common/args.go for the canonical implementation.
func normalizeArgs(args []string) []string {
	if args == nil {
		return nil
	}
	var result []string
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") || arg == "-" || arg == "--" {
			result = append(result, arg)
			continue
		}
		if strings.HasPrefix(arg, "--") {
			arg = "-" + arg[2:]
		}
		if idx := strings.IndexAny(arg[1:], "=:"); idx >= 0 {
			result = append(result, arg[:idx+1], arg[idx+2:])
		} else {
			result = append(result, arg)
		}
	}
	return result
}

// version is set at build time via -ldflags. Format: "<epoch>-<gitsha7>" for dev
// builds, "<epoch>" for release builds. Falls back to embedded catalog epoch.
var version string

// commit is the full 40-char git commit the binary was built from, set at build
// time via `-X main.commit=` (T0854). Empty for binaries built without the
// stamp. The install gate reads it (via `promise version --commit`) to pin its
// test sources to the exact sources the published binary was built from.
var commit string

// timePhases enables per-phase compilation timing output on stderr (--time-phases).
var timePhases bool

// jsonMode makes `promise test` emit newline-delimited JSON test records on
// stdout (one per eligible test) instead of human-readable output, which goes
// to stderr. Set by --json. Only the top-level invocation runs in this mode;
// it fans out children with -child-roster (NOT --json) so children never
// re-enter the parent fan-out — see childRoster. T0763.
var jsonMode bool

// childRoster is the internal flag the JSON-mode parent passes to each child
// (single-file) process. It makes the child print a roster marker line up
// front (so the parent can attribute excluded/not-run tests) but otherwise run
// the normal single-file path. It deliberately does NOT trigger jsonMode, so a
// child never recurses into the multi-file fan-out. T0763.
var childRoster bool

// timePhase prints a single phase timing line to stderr if --time-phases is active.
func timePhase(name string, elapsed time.Duration, extra string) {
	if !timePhases {
		return
	}
	ms := elapsed.Milliseconds()
	if extra != "" {
		fmt.Fprintf(os.Stderr, "[time] %-11s %5dms %s\n", name+":", ms, extra)
	} else {
		fmt.Fprintf(os.Stderr, "[time] %-11s %5dms\n", name+":", ms)
	}
}

// timeSubPhase prints an indented sub-phase timing line (T0215).
func timeSubPhase(name string, elapsed time.Duration, extra string) {
	if !timePhases {
		return
	}
	ms := elapsed.Milliseconds()
	if extra != "" {
		fmt.Fprintf(os.Stderr, "[time]   %-13s %5dms  %s\n", name+":", ms, extra)
	} else {
		fmt.Fprintf(os.Stderr, "[time]   %-13s %5dms\n", name+":", ms)
	}
}

// moduleTimings aggregates timing data collected during module loading (T0215).
type moduleTimings struct {
	parseTime   time.Duration
	semaTime    time.Duration
	files       int
	timings     sema.SemaTimings
	parseCached bool // T0214: true if std AST was loaded from cache
}

//go:embed resources/catalog.toml
var embeddedCatalog []byte

//go:embed all:resources/modules
var embeddedModules embed.FS

//go:embed resources/.sources.sha256
var embeddedSourcesChecksum []byte

//go:embed resources/language-guide.md
var embeddedGuide []byte

//go:embed all:resources/examples
var embeddedExamples embed.FS

// Runtime is fully codegen-emitted LLVM IR — no embedded C files needed.
//
// The CLI's command index, hierarchical help tree, and stream-routing rules
// live in commands.go (T1006). Requested help → stdout/exit 0; usage errors →
// stderr/exit ≠ 0 with a short pointer to `--help`.

func printVersion() {
	v := version
	if v == "" {
		// Fallback: use embedded catalog epoch.
		if epoch, err := module.CompilerEpoch(embeddedCatalog); err == nil {
			v = epoch
		} else {
			v = "unknown"
		}
	}
	if commit != "" {
		fmt.Printf("promise version %s (commit %s)\n", v, commit)
		return
	}
	fmt.Printf("promise version %s\n", v)
}

func main() {
	if len(os.Args) < 2 {
		// If stdin is piped, treat as inline exec
		if info, err := os.Stdin.Stat(); err == nil && info.Mode()&os.ModeCharDevice == 0 {
			runExec(nil)
			return
		}
		// Naked `promise` (interactive) → concise grouped command index (T1006).
		printIndex(os.Stdout)
		return
	}

	// Normalize all args: --flag → -flag, -flag=val → ["-flag", "val"].
	os.Args = append(os.Args[:1], normalizeArgs(os.Args[1:])...)
	cmd := os.Args[1]

	// -version / -help flags (before legacy dispatch).
	if cmd == "-version" {
		printVersion()
		return
	}
	// Root help flag → the command index (the root node's help). The `help`
	// command (handled below) shows the longer overview.
	if cmd == "-help" || cmd == "-h" {
		printIndex(os.Stdout)
		return
	}

	// Centralized help interception: every command/subcommand responds to
	// -h/-help/--help, and `promise help <path...>` routes to any node — all to
	// stdout, exit 0 (T1006).
	if handleHelp(os.Args[1:]) {
		return
	}

	// Legacy flag-based interface for backwards compatibility
	if strings.HasPrefix(cmd, "-") {
		runLegacy(os.Args[1:])
		return
	}

	// Direct-invoke epoch-mismatch warning (T0770): the compiler is no longer a
	// trampoline, so a project-operating command run on the wrong epoch warns.
	if warnEpochCommands[cmd] {
		warnEpochMismatch()
	}

	switch cmd {
	case "version":
		// `promise version --commit` prints the bare build-commit SHA (machine
		// hook for the install gate, T0854). Args are normalized so --commit →
		// -commit. Prints an empty line when the binary carries no stamp.
		if len(os.Args) > 2 && os.Args[2] == "-commit" {
			fmt.Println(commit)
			return
		}
		printVersion()
		return
	case "help":
		// Help with a help flag is handled by handleHelp above; a bare
		// `promise help` or `promise help <path...>` routes through routeHelp.
		routeHelp(os.Args[2:])
		return
	case "guide":
		runGuide(os.Args[2:])
		return
	case "examples":
		runExamples(os.Args[2:])
		return
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
	case "emit-ir":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: promise emit-ir [-target triple] <file.pr>")
			os.Exit(1)
		}
		runEmitIR(os.Args[2:])
	case "ast":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: promise ast <file.pr>")
			os.Exit(1)
		}
		file := parseSourceFile(os.Args[2])
		ast.Print(os.Stdout, file)
	case "exec":
		runExec(os.Args[2:])
	case "format":
		runFmt(os.Args[2:])
	case "doc":
		runDoc(os.Args[2:])
	case "init":
		runInit(os.Args[2:])
	case "clean":
		runClean(os.Args[2:])
	case "package":
		runPackage(os.Args[2:])
	case "pkg", "add", "search", "pin": // deprecated aliases — hidden from usage/help
		runLegacyPackageAlias(cmd, os.Args[2:])
	case "update":
		runUpdate(os.Args[2:])
	case "install":
		runInstall(os.Args[2:])
	case "catalog":
		runCatalog(os.Args[2:])
	case "epochs":
		runEpochs()
	case "use":
		runUse(os.Args[2:])
	case "remove":
		runRemove(os.Args[2:])
	case "fetch", "warm":
		runFetch(os.Args[2:])
		return
	case "gc":
		runGC(os.Args[2:])
		return
	case "bind":
		runBind(os.Args[2:])
	case "doctor":
		runDoctor(os.Args[2:])
		return
	case "targets":
		runTargets(os.Args[2:])
		return
	default:
		// Try treating as a filename for backwards compatibility
		if strings.HasSuffix(cmd, ".pr") {
			runLegacy(os.Args[1:])
			return
		}
		display := cmd
		if len(display) > 50 {
			display = display[:50] + "..."
		}
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", display)
		helpHint(os.Stderr)
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
		fmt.Fprintln(os.Stderr, "error: no input file")
		helpHint(os.Stderr)
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
		fmt.Println(tree.ToStringTree(nil, p))
	}
}

// runEmitIR compiles a .pr file and prints the generated LLVM IR to stdout.
func runEmitIR(args []string) {
	var filename, target string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-target":
			if i+1 < len(args) {
				target = args[i+1]
				i++
			}
		default:
			filename = args[i]
		}
	}
	if filename == "" {
		fmt.Fprintln(os.Stderr, "usage: promise emit-ir [-target triple] <file.pr>")
		os.Exit(1)
	}
	checkTargetFlag(target)
	var file *ast.File
	var info *sema.Info
	if stat, err := os.Stat(filename); err == nil && stat.IsDir() {
		cfg, files, err := discoverProject(filename)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if cfg != nil {
			file, info = compileProjectFrontend(cfg.Dir, files, target)
		} else {
			discovered, err := discoverMainFile(filename)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			file, info = compileFrontendForTarget(discovered, target)
		}
	} else {
		file, info = compileFrontendForTarget(filename, target)
	}
	result := codegen.Compile(file, info, target)
	fmt.Print(result.Module.String())
}

// mainFuncRe matches a top-level main() function declaration in Promise source.
// Matches: main() { ... }, main!() { ... }, main() Type { ... }, main() ! { ... }
// Avoids matching: comments, strings, or nested/indented declarations.
var mainFuncRe = regexp.MustCompile(`(?m)^main\s*!?\s*\(`)

// discoverProject returns the project config and full list of non-test .pr
// source files when dir contains a promise.toml. When there is no promise.toml
// it returns (nil, nil, nil) so callers can fall back to single-file discovery.
// When promise.toml exists but the directory has no .pr files, an error is
// returned so the user is told their project is empty.
func discoverProject(dir string) (*module.Config, []string, error) {
	tomlPath := filepath.Join(dir, "promise.toml")
	if _, err := os.Stat(tomlPath); err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	cfg, err := module.ParseConfig(tomlPath)
	if err != nil {
		return nil, nil, err
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, nil, err
	}
	cfg.Dir = absDir
	files, err := module.CollectModuleSources(absDir, false)
	if err != nil {
		return nil, nil, err
	}
	if len(files) == 0 {
		return nil, nil, fmt.Errorf("error: project %q contains no .pr files", cfg.Name)
	}
	return cfg, files, nil
}

// discoverMainFile finds the entry point .pr file for a project directory.
// Discovery rules (in order):
//  1. promise.toml "main" field → use that file
//  2. Scan .pr files in directory for main() → use if exactly one
//  3. Multiple main() files → error listing them
//  4. No main() files → error
func discoverMainFile(dir string) (string, error) {
	// Rule 1: check promise.toml for explicit main field
	mainField, err := module.FindProjectMain(dir)
	if err != nil {
		return "", err
	}
	if mainField != "" {
		path := filepath.Join(dir, mainField)
		if _, err := os.Stat(path); err != nil {
			return "", fmt.Errorf("error: main file %q (from promise.toml) not found", mainField)
		}
		return path, nil
	}

	// Rule 2-4: scan .pr files for main() function
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("error: cannot read directory %s: %w", dir, err)
	}

	var candidates []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pr") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if mainFuncRe.Match(data) {
			candidates = append(candidates, e.Name())
		}
	}

	switch len(candidates) {
	case 1:
		return filepath.Join(dir, candidates[0]), nil
	case 0:
		return "", fmt.Errorf("error: no main() function found in project\nhint: add a main() function or specify a file: promise build file.pr")
	default:
		var b strings.Builder
		b.WriteString("error: multiple files contain main() — specify which to use:")
		for _, f := range candidates {
			b.WriteString("\n  ")
			b.WriteString(f)
		}
		b.WriteString("\nhint: add 'main = \"")
		b.WriteString(candidates[0])
		b.WriteString("\"' to promise.toml")
		return "", fmt.Errorf("%s", b.String())
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
	debugMode := false
	releaseMode := false
	componentMode := false
	adaptPath := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-o":
			if i+1 < len(args) {
				outputFile = args[i+1]
				i++
			}
		case "-target":
			if i+1 < len(args) {
				target = args[i+1]
				i++
			}
		case "-component":
			componentMode = true
		case "-adapt":
			if i+1 < len(args) {
				adaptPath = args[i+1]
				i++
			}
		case "-debug":
			debugMode = true
		case "-release":
			releaseMode = true
		case "-time-phases":
			timePhases = true
		default:
			filename = args[i]
		}
	}

	if debugMode && releaseMode {
		fmt.Fprintln(os.Stderr, "error: -debug and -release are mutually exclusive")
		os.Exit(1)
	}

	checkTargetFlag(target)

	// Auto-discover main file: no arg → CWD, directory arg → that dir (T0115).
	// When the directory contains a promise.toml, switch to project mode and
	// compile every .pr file in the tree as a single program (T0492).
	var projectDir string
	var projectCfg *module.Config
	var projectFiles []string

	resolveDir := func(dir string) {
		cfg, files, err := discoverProject(dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if cfg != nil {
			projectDir = cfg.Dir
			projectCfg = cfg
			projectFiles = files
			return
		}
		discovered, err := discoverMainFile(dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		filename = discovered
	}

	if filename == "" {
		resolveDir(".")
	} else if info, err := os.Stat(filename); err == nil {
		if info.IsDir() {
			resolveDir(filename)
		} else if projRoot := findEnclosingProjectDir(filename); projRoot != "" {
			fmt.Fprintf(os.Stderr,
				"note: %s belongs to the project at %s — building the whole project "+
					"(run `promise build` to build it directly)\n", filename, projRoot)
			resolveDir(projRoot)
		}
	}
	// A nonexistent filename falls through to compileFrontend below, which
	// reports a clear file-not-found error rather than silently building the
	// enclosing project for a name that doesn't exist (T0927).

	if target == "" {
		target = codegen.HostTargetTriple()
	}

	if componentMode && !isWasmTarget(target) {
		fmt.Fprintln(os.Stderr, "error: -component requires a WASM target (e.g. -target wasm32-wasi)")
		os.Exit(1)
	}

	if outputFile == "" {
		var base string
		if projectCfg != nil {
			base = projectCfg.Name
		} else {
			base = strings.TrimSuffix(filepath.Base(filename), ".pr")
		}
		if isWasmTarget(target) {
			if componentMode {
				outputFile = base + ".component.wasm"
			} else {
				outputFile = base + ".wasm"
			}
		} else if isWindowsTarget(target) {
			outputFile = base + ".exe"
		} else {
			outputFile = base
		}
	}

	// Default to debug mode (scribble malloc'd + poison freed memory for UAF / uninit-read detection).
	// Use -release for production builds with platform-default free behavior.
	debugAllocator := !releaseMode

	var compileStart time.Time
	if timePhases {
		compileStart = time.Now()
	}

	var file *ast.File
	var info *sema.Info
	if projectCfg != nil {
		file, info = compileProjectFrontend(projectDir, projectFiles, target)
	} else {
		file, info = compileFrontend(filename)
	}

	// Check for main() function — must exist for build/run (not test).
	if !hasMainFunc(info) {
		fmt.Fprintf(os.Stderr, "error: program has no main() function\n\n"+
			"A runnable program requires a main() function as its entry point:\n\n"+
			"  main() {\n"+
			"    // your code here\n"+
			"  }\n")
		os.Exit(1)
	}

	tCodegen := time.Now()
	result := codegen.CompileWithOptions(file, info, target, &codegen.CompileOptions{
		CachedInstances: lookupCachedInstances(info, target, buildModeStr(releaseMode)),
		DebugAllocator:  debugAllocator,
	})
	timePhase("codegen", time.Since(tCodegen), "")

	if componentMode {
		// Component mode: link to a temp core wasm, then wrap with wasm-tools.
		coreWasm := outputFile + ".core"
		compileAndLink(result, coreWasm, target, filename, releaseMode)
		defer os.Remove(coreWasm)
		componentWrap(coreWasm, outputFile, adaptPath)
	} else {
		compileAndLink(result, outputFile, target, filename, releaseMode)
	}

	if timePhases {
		timePhase("total", time.Since(compileStart), "")
	}
	if projectCfg != nil {
		// Report the project directory in the "Compiled X → Y" message rather
		// than an empty source path.
		filename = projectDir
	}
	return filename, outputFile, target
}

// hasMainFunc checks whether the sema info contains a main() function in the file scope.
func hasMainFunc(info *sema.Info) bool {
	if len(info.ScopeOrder) == 0 {
		return false
	}
	obj := info.ScopeOrder[0].Lookup("main")
	if obj == nil {
		return false
	}
	_, isFunc := obj.(*types.Func)
	return isFunc
}

// parseRunArgs extracts the filename, target triple, and releaseMode from
// `promise run` arguments using the same last-wins semantics as buildToFile,
// so the cache key aligns with what actually gets compiled.
func parseRunArgs(args []string) (filename, target string, releaseMode bool) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-o":
			// Consume the arg value so it isn't misread as the filename.
			if i+1 < len(args) {
				i++
			}
		case "-target":
			if i+1 < len(args) {
				target = args[i+1]
				i++
			}
		case "-release":
			releaseMode = true
		case "-debug", "-time-phases":
			// no-op for cache key: -debug is default; -time-phases doesn't affect output
		default:
			filename = args[i]
		}
	}
	return
}

// runRun compiles and immediately runs a .pr file. The compiled binary is
// cached (keyed by source + compiler + std + target + mode + embeds + local
// deps) under ~/.promise/cache/build/, so repeat runs on the same source skip
// compilation entirely (T0152).
func runRun(args []string) {
	// Parse args locally to compute the cache key before invoking buildToFile.
	// buildToFile reparses the same args on cache miss; that's cheap.
	filename, target, releaseMode := parseRunArgs(args)

	// Validate -target here so we reject bogus values before acquiring the
	// build-cache lock, computing a cache key, or creating a temp file (which
	// os.Exit would leak — it doesn't run deferreds).
	checkTargetFlag(target)

	// Resolve the target binary to what buildToFile would actually compile so the
	// cache key matches across invocations (T0115 auto-discovery, T0492 project mode).
	var projectDir string
	resolveDir := func(dir string) {
		if cfg, _, err := discoverProject(dir); err == nil && cfg != nil {
			projectDir = cfg.Dir
			return
		}
		if discovered, err := discoverMainFile(dir); err == nil {
			filename = discovered
		}
	}
	if filename == "" {
		resolveDir(".")
	} else if info, err := os.Stat(filename); err == nil {
		if info.IsDir() {
			resolveDir(filename)
		} else if projRoot := findEnclosingProjectDir(filename); projRoot != "" {
			// A file inside a project builds the whole project (T0927); align the
			// cache key with the project so it doesn't go stale when siblings change.
			if cfg, _, err := discoverProject(projRoot); err == nil && cfg != nil {
				projectDir = cfg.Dir
			}
		}
	}

	if target == "" {
		target = codegen.HostTargetTriple()
	}

	// Shared lock so promise clean can't wipe the cache mid-lookup.
	unlock := module.LockBuildDirShared()
	defer unlock()

	var cacheKey string
	var cacheable bool
	var cacheDir string
	cacheLabel := filename
	if projectDir != "" {
		cacheKey, cacheable = computeProjectBinaryCacheKey(projectDir, target, releaseMode)
		cacheLabel = projectDir
	} else if filename != "" {
		cacheKey, cacheable = computeRunBinaryCacheKey(filename, target, releaseMode)
	}
	if cacheable {
		cacheDir, _ = module.BuildCacheDir()
	}

	// Cache hit — exec directly, no compile.
	if cacheDir != "" {
		if cachedBin := module.LookupTestBinaryCache(cacheDir, cacheKey); cachedBin != "" {
			if os.Getenv("PROMISE_CACHE_DEBUG") != "" {
				fmt.Fprintf(os.Stderr, "[cache HIT] %s key=%s\n", filepath.Base(cacheLabel), cacheKey[:16])
			}
			execRunBinary(cachedBin)
			return
		}
	}

	if os.Getenv("PROMISE_CACHE_DEBUG") != "" {
		if cacheable {
			fmt.Fprintf(os.Stderr, "[cache MISS] %s key=%s compiler=%s std=%s target=%s\n",
				filepath.Base(cacheLabel), cacheKey[:16], module.CompilerHash()[:16], cachedStdHash()[:16], target)
			if os.Getenv("PROMISE_CACHE_DEBUG") == "verbose" {
				var inputs []module.CacheKeyInput
				if projectDir != "" {
					inputs = computeProjectBinaryCacheInputs(projectDir, target, releaseMode)
				} else {
					inputs = computeRunBinaryCacheInputs(filename, target, releaseMode)
				}
				fmt.Fprintln(os.Stderr, module.FormatCacheKeyInputs(
					"run-binary "+filepath.Base(cacheLabel), cacheKey, inputs))
			}
		} else if cacheLabel != "" {
			fmt.Fprintf(os.Stderr, "[cache SKIP] %s (not cacheable)\n", filepath.Base(cacheLabel))
		}
	}

	// Cache miss — compile to a temp file.
	ext := binaryExtension(codegen.HostTargetTriple())
	tmpOutput, err := os.CreateTemp("", "promise-run-*"+ext)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating temp file: %v\n", err)
		os.Exit(1)
	}
	tmpOutput.Close()
	defer os.Remove(tmpOutput.Name())

	buildArgs := []string{"-o", tmpOutput.Name()}
	buildArgs = append(buildArgs, args...)
	buildToFile(buildArgs)

	// Save the compiled binary to the cache for future runs.
	if cacheDir != "" {
		if err := module.SaveTestBinaryCache(cacheDir, cacheKey, tmpOutput.Name()); err == nil {
			module.SaveTestBinaryMeta(cacheDir, cacheKey, &module.CacheMeta{
				Kind:     module.CacheKindBinary,
				Name:     cacheLabel,
				CacheKey: cacheKey,
			})
		}
	}

	execRunBinary(tmpOutput.Name())
}

// execRunBinary runs the given binary with the current process's stdio wired
// through, and exits with the child's exit code on failure.
func execRunBinary(path string) {
	cmd := exec.Command(path)
	isolateProcessGroup(cmd)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(sanitizeExitCode(exitErr.ExitCode()))
		}
		fmt.Fprintf(os.Stderr, "error running: %v\n", err)
		os.Exit(1)
	}
}

// defaultMemoryLimitBytes is the conservative ceiling applied per-test process
// when -memory-limit is not specified (T0689). 2 GB is comfortably above what
// any legitimate test in the suite needs while still being far below total RAM,
// so a runaway allocation fails the offending test rather than driving the
// machine into swap. The suite passes clean at 2 GiB; lower this once the
// suite peak has been measured (multi-file forwarding fixed in T0738, so a
// future bracketing run via -memory-limit can measure it accurately).
const defaultMemoryLimitBytes int64 = 2 << 30 // 2 GiB

// testTimeoutConfig holds CLI timeout/memory-limit configuration for per-test
// resolution.
type testTimeoutConfig struct {
	defaultTimeout      time.Duration // -timeout (default 60s)
	scale               float64       // -timeout-scale (default 1.0)
	min                 time.Duration // -timeout-min (0 = no minimum)
	max                 time.Duration // -timeout-max (0 = no maximum)
	compileTimeout      time.Duration // -compile-timeout (default 10m) — backstop for hung compilation
	defaultMemoryBytes  int64         // -memory-limit (default 2 GiB; 0 = disabled)
	memoryLimitExplicit bool          // true if -memory-limit was passed on the CLI
}

// cacheString returns a stable string representation of the timeout config
// for inclusion in cache keys. Per-test timeouts are baked into test binaries
// at compile time, so the cache key must change when timeout config changes.
func (c testTimeoutConfig) cacheString() string {
	return fmt.Sprintf("timeout:%d,scale:%.10g,min:%d,max:%d,memlimit:%d",
		c.defaultTimeout.Nanoseconds(), c.scale,
		c.min.Nanoseconds(), c.max.Nanoseconds(),
		c.defaultMemoryBytes)
}

// computeTestTimeouts computes the final per-test timeout in nanoseconds for each test.
// Resolution: final = clamp((annotation ?: default) × scale, min, max)
func computeTestTimeouts(tests []*types.Func, info *sema.Info, cfg testTimeoutConfig) map[string]int64 {
	result := make(map[string]int64, len(tests))
	for _, t := range tests {
		base := cfg.defaultTimeout
		if raw, ok := info.TestTimeouts[t.Name()]; ok {
			if d, err := time.ParseDuration(raw); err == nil {
				base = d
			}
		}
		final := time.Duration(float64(base) * cfg.scale)
		if cfg.min > 0 && final < cfg.min {
			final = cfg.min
		}
		if cfg.max > 0 && final > cfg.max {
			final = cfg.max
		}
		result[t.Name()] = final.Nanoseconds()
	}
	return result
}

// computeTestMemoryLimits computes the final per-test memory limit in bytes
// for each test. Resolution: annotation ?: cfg.defaultMemoryBytes. Returns nil
// if memory limit accounting is disabled (cfg.defaultMemoryBytes == 0 AND no
// test in the file has a memory_limit annotation). T0689.
func computeTestMemoryLimits(tests []*types.Func, info *sema.Info, cfg testTimeoutConfig) map[string]int64 {
	// First pass: determine whether any test opts in via annotation.
	anyAnnotation := false
	if info.TestMemoryLimits != nil {
		for _, t := range tests {
			if _, ok := info.TestMemoryLimits[t.Name()]; ok {
				anyAnnotation = true
				break
			}
		}
	}
	if cfg.defaultMemoryBytes == 0 && !anyAnnotation {
		return nil
	}
	result := make(map[string]int64, len(tests))
	for _, t := range tests {
		limit := cfg.defaultMemoryBytes
		if info.TestMemoryLimits != nil {
			if raw, ok := info.TestMemoryLimits[t.Name()]; ok {
				if v, err := parseMemoryLimitArg(raw); err == nil {
					limit = v
				}
			}
		}
		result[t.Name()] = limit
	}
	return result
}

// computeE2ETimeout computes the timeout for an e2e test based on annotation and config.
func computeE2ETimeout(info *sema.Info, cfg testTimeoutConfig) time.Duration {
	base := cfg.defaultTimeout
	if raw, ok := info.TestTimeouts["main"]; ok {
		if d, err := time.ParseDuration(raw); err == nil {
			base = d
		}
	}
	final := time.Duration(float64(base) * cfg.scale)
	if cfg.min > 0 && final < cfg.min {
		final = cfg.min
	}
	if cfg.max > 0 && final > cfg.max {
		final = cfg.max
	}
	return final
}

// runTest discovers and runs `test annotated functions.
// Accepts a single file, a directory (non-recursive), or dir/... (recursive).
func runTest(args []string) {
	timeout := 60 * time.Second
	parallel := runtime.NumCPU()
	timeoutScale := 1.0
	var timeoutMin time.Duration // 0 = no minimum
	var timeoutMax time.Duration // 0 = no maximum
	var stressMode bool
	var stressCount int                         // 0 = unlimited
	var stressDuration time.Duration            // 0 = unlimited
	var targetTriple string                     // empty = host target
	var outputFile string                       // stress report output file
	var coverageMode bool                       // T0030: coverage instrumentation
	var reportJSON string                       // T0749: passing-test report path (parent-only)
	compileTimeout := 10 * time.Minute          // -compile-timeout (backstop for hung compilation)
	memoryLimitBytes := defaultMemoryLimitBytes // T0689: default 2 GiB ceiling per test process
	memoryLimitExplicit := false                // whether -memory-limit was passed
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
		} else if args[i] == "-timeout-scale" && i+1 < len(args) {
			f, err := strconv.ParseFloat(args[i+1], 64)
			if err != nil || f <= 0 {
				fmt.Fprintln(os.Stderr, "error: -timeout-scale requires a positive number")
				os.Exit(1)
			}
			timeoutScale = f
			i++
		} else if args[i] == "-timeout-min" && i+1 < len(args) {
			d, err := parseTimeoutArg(args[i+1])
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			timeoutMin = d
			i++
		} else if args[i] == "-timeout-max" && i+1 < len(args) {
			d, err := parseTimeoutArg(args[i+1])
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			timeoutMax = d
			i++
		} else if args[i] == "-parallel" && i+1 < len(args) {
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < 1 {
				fmt.Fprintln(os.Stderr, "error: -parallel requires a positive integer")
				os.Exit(1)
			}
			parallel = n
			i++
		} else if args[i] == "-target" && i+1 < len(args) {
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
		} else if args[i] == "-output" && i+1 < len(args) {
			outputFile = args[i+1]
			i++
		} else if args[i] == "-compile-timeout" && i+1 < len(args) {
			d, err := parseTimeoutArg(args[i+1])
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			compileTimeout = d
			i++
		} else if args[i] == "-memory-limit" && i+1 < len(args) {
			n, err := parseMemoryLimitArg(args[i+1])
			if err != nil {
				fmt.Fprintln(os.Stderr, "error: "+err.Error())
				os.Exit(1)
			}
			memoryLimitBytes = n
			memoryLimitExplicit = true
			i++
		} else if args[i] == "-coverage" {
			coverageMode = true
		} else if args[i] == "-report-json" && i+1 < len(args) {
			reportJSON = args[i+1]
			i++
		} else if args[i] == "-time-phases" {
			timePhases = true
		} else if args[i] == "--json" || args[i] == "-json" {
			jsonMode = true
		} else if args[i] == "-child-roster" {
			childRoster = true
		} else {
			remaining = append(remaining, args[i])
		}
	}

	if len(remaining) < 1 {
		fmt.Fprintln(os.Stderr, "usage: promise test [-timeout duration] [-timeout-scale N] [-timeout-min duration] [-timeout-max duration] [-compile-timeout duration] [-parallel N] [-stress [N|duration]] [-output file] [-coverage] [-report-json file] [-time-phases] <file.pr | dir | dir/...> ...")
		os.Exit(1)
	}

	checkTargetFlag(targetTriple)

	// Expand all targets into a flat file list.
	var allFiles []string
	for _, arg := range remaining {
		target := arg
		recursive := false
		if strings.HasSuffix(target, "/...") || target == "..." {
			recursive = true
			if target == "..." {
				target = "."
			} else {
				target = strings.TrimSuffix(target, "/...")
			}
		}

		info, err := os.Stat(target)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}

		if info.IsDir() {
			allFiles = append(allFiles, discoverTestFiles(target, recursive)...)
		} else {
			allFiles = append(allFiles, target)
		}
	}

	cfg := testTimeoutConfig{
		defaultTimeout:      timeout,
		scale:               timeoutScale,
		min:                 timeoutMin,
		max:                 timeoutMax,
		compileTimeout:      compileTimeout,
		defaultMemoryBytes:  memoryLimitBytes,
		memoryLimitExplicit: memoryLimitExplicit,
	}

	if stressMode {
		runStress(allFiles, stressCount, stressDuration, cfg, targetTriple, outputFile)
		return
	}

	// Single file: use simple runner (no directory summary).
	// Multiple files: combined summary at the end.
	// JSON mode always uses the multi-file runner — it owns the JSONL emission.
	if len(allFiles) == 1 && !jsonMode {
		runTestFile(allFiles[0], cfg, targetTriple, coverageMode)
	} else {
		runTestFiles(allFiles, cfg, targetTriple, parallel, coverageMode, reportJSON)
	}
}

// runTestFile runs test functions from a single .pr file.
// targetTriple overrides the compilation target (empty = host).
func runTestFile(filename string, cfg testTimeoutConfig, targetTriple string, coverageMode bool) {
	start := time.Now()
	// For cache-hit paths where we can't compute per-test timeouts,
	// use the CLI default as the process-level timeout.
	timeout := cfg.defaultTimeout

	// Module test dispatch: compile all module sources + tests together,
	// with build cache support.
	if modDir := isModuleTestFile(filename); modDir != "" {
		runModuleTestFile(modDir, cfg, start, targetTriple, coverageMode)
		return
	}

	// Test binary cache for non-module files.
	target := targetTriple
	if target == "" {
		target = codegen.HostTargetTriple()
	}

	// Coverage mode: skip cache (instrumented IR differs), compile with coverage,
	// and parse coverage data from output (T0030).
	if coverageMode {
		file, info := compileFrontendForTarget(filename, targetTriple)
		if len(info.Tests) == 0 {
			fmt.Println("no tests found")
			return
		}
		emitRoster("batch", testNames(info.Tests), info.TestExcludes, nil, target)
		testTimeouts := computeTestTimeouts(info.Tests, info, cfg)
		testMemoryLimits := computeTestMemoryLimits(info.Tests, info, cfg)
		binaryPath, regions := compileTestBinaryWithCoverage(file, info, targetTriple, filename, testTimeouts, testMemoryLimits)
		defer os.Remove(binaryPath)
		var totalNs int64
		for _, ns := range testTimeouts {
			totalNs += ns
		}
		processTimeout := time.Duration(totalNs) + 30*time.Second
		runTestBinaryWithCoverage(binaryPath, processTimeout, start, targetTriple, regions)
		return
	}

	cacheKey, cacheable := computeTestFileCacheKey(filename, target, cfg)
	var cacheDir string
	if cacheable {
		cacheDir, _ = module.BuildCacheDir()
	}

	// Check cache.
	if cacheDir != "" {
		if cachedBin := module.LookupTestBinaryCache(cacheDir, cacheKey); cachedBin != "" {
			if os.Getenv("PROMISE_CACHE_DEBUG") != "" {
				fmt.Fprintf(os.Stderr, "[cache HIT] %s key=%s\n", filepath.Base(filename), cacheKey[:16])
			}
			meta := module.LoadTestBinaryMeta(cacheDir, cacheKey)
			// Use saved process timeout if available; fall back to
			// scaling by test count for older cache entries.
			if meta != nil && meta.ProcessTimeoutNs > 0 {
				timeout = time.Duration(meta.ProcessTimeoutNs)
			} else if meta != nil && len(meta.Tests) > 1 {
				timeout = cfg.defaultTimeout*time.Duration(len(meta.Tests)) + 30*time.Second
			}
			if meta != nil && meta.E2E {
				emitRoster("e2e", []string{"main"}, nil, meta.ExcludeTargets, target)
				executeE2EBinary(cachedBin, meta.ExpectedOutput, meta.ExcludeTargets,
					filename, timeout, start, targetTriple)
			} else {
				if meta != nil {
					emitRoster("batch", meta.Tests, meta.TestExcludes, nil, target)
				}
				runTestBinary(cachedBin, timeout, start, targetTriple)
			}
			return
		}
	}

	// Cache miss — compile.
	if os.Getenv("PROMISE_CACHE_DEBUG") != "" {
		if cacheable {
			fmt.Fprintf(os.Stderr, "[cache MISS] %s key=%s compiler=%s std=%s target=%s\n",
				filepath.Base(filename), cacheKey[:16], module.CompilerHash()[:16], cachedStdHash()[:16], target)
			if os.Getenv("PROMISE_CACHE_DEBUG") == "verbose" {
				inputs := computeTestFileCacheInputs(filename, target, cfg)
				fmt.Fprintln(os.Stderr, module.FormatCacheKeyInputs(
					"test-file "+filepath.Base(filename), cacheKey, inputs))
			}
		} else {
			fmt.Fprintf(os.Stderr, "[cache SKIP] %s (not cacheable)\n", filepath.Base(filename))
		}
	}
	var compileStart time.Time
	if timePhases {
		compileStart = time.Now()
	}

	file, info := compileFrontendForTarget(filename, targetTriple)

	if info.HasExpectOutput {
		emitRoster("e2e", []string{"main"}, nil, info.ExcludeTargets, target)
		e2eTimeout := computeE2ETimeout(info, cfg)
		runE2ETest(file, info, filename, e2eTimeout, start, targetTriple, cacheDir, cacheKey, compileStart)
		return
	}

	if len(info.Tests) == 0 {
		fmt.Println("no tests found")
		return
	}

	emitRoster("batch", testNames(info.Tests), info.TestExcludes, nil, target)

	testTimeouts := computeTestTimeouts(info.Tests, info, cfg)
	testMemoryLimits := computeTestMemoryLimits(info.Tests, info, cfg)
	binaryPath := compileTestBinary(file, info, targetTriple, filename, testTimeouts, testMemoryLimits)

	if timePhases {
		timePhase("total", time.Since(compileStart), "")
	}

	defer os.Remove(binaryPath)
	// Process-level timeout: sum of all per-test timeouts + 30s buffer.
	// Per-test timeouts are enforced in-binary; this is just a backstop.
	var totalNs int64
	for _, ns := range testTimeouts {
		totalNs += ns
	}
	processTimeout := time.Duration(totalNs) + 30*time.Second

	// Save to cache (includes process timeout for cache-hit path).
	if cacheDir != "" {
		module.SaveTestBinaryCache(cacheDir, cacheKey, binaryPath)
		var testNames []string
		testExcludes := map[string][]string{}
		for _, t := range info.Tests {
			testNames = append(testNames, t.Name())
			if excludes, ok := info.TestExcludes[t.Name()]; ok {
				testExcludes[t.Name()] = excludes
			}
		}
		module.SaveTestBinaryMeta(cacheDir, cacheKey, &module.CacheMeta{
			Kind:             module.CacheKindBinary,
			Name:             filename,
			CacheKey:         cacheKey,
			Tests:            testNames,
			TestExcludes:     testExcludes,
			ProcessTimeoutNs: processTimeout.Nanoseconds(),
		})
	}

	runTestBinary(binaryPath, processTimeout, start, targetTriple)
}

// runModuleTestFile compiles and runs a module's test suite. All module source
// files (including all *_test.pr) are compiled together as a single unit.
// Test binaries are cached in the build cache for fast re-runs.
func runModuleTestFile(modDir string, cfg testTimeoutConfig, start time.Time, targetTriple string, coverageMode bool) {
	target := targetTriple
	if target == "" {
		target = codegen.HostTargetTriple()
	}

	// Coverage mode: skip cache, compile with coverage instrumentation.
	if coverageMode {
		file, info := compileModuleTestFrontend(modDir, targetTriple)
		if len(info.Tests) == 0 {
			fmt.Println("no tests found")
			return
		}
		emitRoster("batch", testNames(info.Tests), info.TestExcludes, nil, target)
		testTimeouts := computeTestTimeouts(info.Tests, info, cfg)
		testMemoryLimits := computeTestMemoryLimits(info.Tests, info, cfg)
		binaryPath, regions := compileTestBinaryWithCoverage(file, info, targetTriple, modDir, testTimeouts, testMemoryLimits)
		defer os.Remove(binaryPath)
		var totalNs int64
		for _, ns := range testTimeouts {
			totalNs += ns
		}
		processTimeout := time.Duration(totalNs) + 30*time.Second
		runTestBinaryWithCoverage(binaryPath, processTimeout, start, targetTriple, regions)
		return
	}

	// Check build cache for a cached test binary.
	implHash, err := module.HashModuleSources(modDir, true) // includes test files
	if err != nil {
		fmt.Fprintf(os.Stderr, "error hashing module sources: %v\n", err)
		os.Exit(1)
	}
	// Include embedded file contents so data file changes invalidate the
	// test binary cache (T0032).
	if embedHash := module.HashModuleEmbeds(modDir, true); embedHash != "" {
		eh := fnv.New128a()
		fmt.Fprintf(eh, "%s\n%s", implHash, embedHash)
		implHash = hex.EncodeToString(eh.Sum(nil))
	}
	compilerHash := module.CompilerHash()
	// Include timeout config in the cache key since per-test timeouts are
	// baked into the test binary at compile time (B0132).
	th := fnv.New128a()
	fmt.Fprintf(th, "%s\n%s", implHash, cfg.cacheString())
	implHashWithTimeout := hex.EncodeToString(th.Sum(nil))

	// T0181: Include dependency hashes in cache key so that changes to
	// imported local modules invalidate the consuming module's cached binary.
	depHashes := scanModuleLocalDeps(modDir)

	cacheKey := module.BuildCacheKey(implHashWithTimeout, compilerHash, target, "debug", depHashes)
	cacheDir, _ := module.BuildCacheDir()

	cacheDebug := os.Getenv("PROMISE_CACHE_DEBUG") != ""

	if cacheDir != "" {
		if cachedBin := module.LookupTestBinaryCache(cacheDir, cacheKey); cachedBin != "" {
			if cacheDebug {
				fmt.Fprintf(os.Stderr, "[cache HIT] %s key=%s\n", filepath.Base(modDir), cacheKey[:16])
			}
			timeout := cfg.defaultTimeout
			meta := module.LoadTestBinaryMeta(cacheDir, cacheKey)
			if meta != nil && meta.ProcessTimeoutNs > 0 {
				timeout = time.Duration(meta.ProcessTimeoutNs)
			} else if meta != nil && len(meta.Tests) > 1 {
				timeout = cfg.defaultTimeout*time.Duration(len(meta.Tests)) + 30*time.Second
			}
			if meta != nil {
				emitRoster("batch", meta.Tests, meta.TestExcludes, nil, target)
			}
			runTestBinary(cachedBin, timeout, start, targetTriple)
			return
		}
	}

	if cacheDebug {
		fmt.Fprintf(os.Stderr, "[cache MISS] %s key=%s compiler=%s target=%s deps=%d\n",
			filepath.Base(modDir), cacheKey[:16], compilerHash[:16], target, len(depHashes))
		if os.Getenv("PROMISE_CACHE_DEBUG") == "verbose" {
			inputs := []module.CacheKeyInput{
				{Label: "impl", Value: implHashWithTimeout},
				{Label: "compiler", Value: compilerHash},
				{Label: "target", Value: target},
			}
			for _, dh := range depHashes {
				parts := strings.SplitN(dh, ":", 2)
				if len(parts) == 2 {
					inputs = append(inputs, module.CacheKeyInput{Label: "dep " + parts[0], Value: parts[1]})
				}
			}
			fmt.Fprintln(os.Stderr, module.FormatCacheKeyInputs(
				"module-test "+filepath.Base(modDir), cacheKey, inputs))
		}
	}

	// Cache miss — compile the module test suite.
	file, info := compileModuleTestFrontend(modDir, targetTriple)

	if len(info.Tests) == 0 {
		fmt.Println("no tests found")
		return
	}

	emitRoster("batch", testNames(info.Tests), info.TestExcludes, nil, target)

	testTimeouts := computeTestTimeouts(info.Tests, info, cfg)
	testMemoryLimits := computeTestMemoryLimits(info.Tests, info, cfg)
	binaryPath := compileTestBinary(file, info, targetTriple, modDir, testTimeouts, testMemoryLimits)

	defer os.Remove(binaryPath)
	// Process-level timeout: sum of per-test timeouts + 30s buffer.
	var totalNs int64
	for _, ns := range testTimeouts {
		totalNs += ns
	}
	processTimeout := time.Duration(totalNs) + 30*time.Second

	// Save compiled binary to cache (includes process timeout for cache-hit path).
	if cacheDir != "" {
		module.SaveTestBinaryCache(cacheDir, cacheKey, binaryPath)
		var names []string
		testExcludes := map[string][]string{}
		for _, t := range info.Tests {
			names = append(names, t.Name())
			if excludes, ok := info.TestExcludes[t.Name()]; ok {
				testExcludes[t.Name()] = excludes
			}
		}
		module.SaveTestBinaryMeta(cacheDir, cacheKey, &module.CacheMeta{
			Kind:             module.CacheKindBinary,
			Name:             modDir,
			CacheKey:         cacheKey,
			Tests:            names,
			TestExcludes:     testExcludes,
			ProcessTimeoutNs: processTimeout.Nanoseconds(),
		})
	}

	runTestBinary(binaryPath, processTimeout, start, targetTriple)
}

// compileTestBinary runs codegen + link for a test file and returns the binary path.
// testTimeouts maps test function names to their computed timeout in nanoseconds.
// testMemoryLimits (nil = no accounting) maps test function names to memory
// limits in bytes (T0689).
func compileTestBinary(file *ast.File, info *sema.Info, targetTriple, sourceFile string, testTimeouts map[string]int64, testMemoryLimits map[string]int64) string {
	target := targetTriple
	if target == "" {
		target = codegen.HostTargetTriple()
	}
	tCodegen := time.Now()
	result := codegen.CompileWithOptions(file, info, target, &codegen.CompileOptions{
		DebugAllocator:        true,                    // tests always use debug mode
		MemoryLimitAccounting: testMemoryLimits != nil, // T0689: emit accounting allocator + helpers when any per-test limit is set
	})
	result.SetTestMemoryLimits(testMemoryLimits)
	result.GenerateTestMain(info.Tests, testTimeouts)
	timePhase("codegen", time.Since(tCodegen), "")

	ext := binaryExtension(target)
	tmpOutput, err := os.CreateTemp("", "promise-test-*"+ext)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating temp file: %v\n", err)
		os.Exit(1)
	}
	tmpOutput.Close()
	compileAndLink(result, tmpOutput.Name(), target, sourceFile, false) // tests always use debug mode
	return tmpOutput.Name()
}

// runTestBinary executes a compiled test binary and prints formatted results.
func runTestBinary(binaryPath string, timeout time.Duration, start time.Time, targetTriple string) {
	target := targetTriple
	if target == "" {
		target = codegen.HostTargetTriple()
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var cmd *exec.Cmd
	switch {
	case isWasmWebTarget(target):
		cmd = runWasmWeb(ctx, binaryPath)
	case isWasmTarget(target):
		cmd = exec.CommandContext(ctx, "wasmtime", binaryPath)
	default:
		cmd = exec.CommandContext(ctx, binaryPath)
	}
	isolateProcessGroup(cmd)
	output, runErr := cmd.CombinedOutput()
	elapsed := time.Since(start)

	if ctx.Err() == context.DeadlineExceeded {
		printTestOutput(string(output))
		fmt.Fprintf(os.Stderr, "TIMEOUT: tests exceeded %s timeout\n", timeout)
		os.Exit(1)
	}

	// T0689: detect the structured `fatal: memory limit exceeded` stderr line
	// emitted by the accounting allocator on abort. When present, the child
	// process called exit(134) mid-batch — no summary was printed. We synthesize
	// a MEMLIMIT line and a summary so the user sees a clean classification.
	memlimitTripped := strings.Contains(string(output), "fatal: memory limit exceeded")

	// Print output: format raw "PASS <ns> <name>" lines and replace summary with timed version
	targetSuffix := ""
	if targetTriple != "" && targetTriple != codegen.HostTargetTriple() {
		targetSuffix = fmt.Sprintf(" [%s]", targetTriple)
	}
	summaryRe := regexp.MustCompile(`^(\d+) passed, (\d+) failed(?:, (\d+) skipped)?(?:, (\d+) leaked)?(?:, (\d+) timed out)?(?:, (\d+) allowed leaks)?(?:, (\d+) stale allow_leaks)?`)
	sawSummary := false
	passedCount := 0
	failedCount := 0
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		// T0689: filter out raw PAL fatal lines — we'll emit a synthetic
		// MEMLIMIT report block at the end.
		if memlimitTripped && strings.HasPrefix(line, "fatal: memory limit exceeded") {
			continue
		}
		if strings.HasPrefix(line, "pass ") {
			passedCount++
		} else if strings.HasPrefix(line, "FAIL ") {
			failedCount++
		}
		if m := summaryRe.FindStringSubmatch(line); m != nil {
			sawSummary = true
			fmt.Println() // empty line before summary
			summary := fmt.Sprintf("%s passed, %s failed", m[1], m[2])
			if m[3] != "" {
				summary += fmt.Sprintf(", %s skipped", m[3])
			}
			if len(m) > 4 && m[4] != "" {
				summary += fmt.Sprintf(", %s leaked", m[4])
			}
			if len(m) > 5 && m[5] != "" {
				summary += fmt.Sprintf(", %s timed out", m[5])
			}
			if len(m) > 6 && m[6] != "" {
				summary += fmt.Sprintf(", %s allowed leaks", m[6])
			}
			if len(m) > 7 && m[7] != "" {
				summary += fmt.Sprintf(", %s stale allow_leaks", m[7])
			}
			fmt.Printf("%s (%.3fs)%s\n", summary, elapsed.Seconds(), targetSuffix)
		} else if targetSuffix != "" && (strings.HasPrefix(line, "pass ") || strings.HasPrefix(line, "FAIL ") || strings.HasPrefix(line, "LEAK ") || strings.HasPrefix(line, "TIMEOUT ") || strings.HasPrefix(line, "MEMLIMIT ")) {
			fmt.Printf("%s%s\n", line, targetSuffix)
		} else {
			fmt.Println(line)
		}
	}

	if memlimitTripped {
		// Synthesize a MEMLIMIT line + summary if the child aborted before
		// emitting its own summary line.
		fmt.Printf("MEMLIMIT (-) <aborted>%s\n", targetSuffix)
		fmt.Println("  memory limit: exceeded (test process aborted; subsequent tests not run)")
		if !sawSummary {
			fmt.Println()
			fmt.Printf("%d passed, %d failed, 1 memlimit (%.3fs)%s\n",
				passedCount, failedCount, elapsed.Seconds(), targetSuffix)
		}
		os.Exit(1)
	}

	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			os.Exit(sanitizeExitCode(exitErr.ExitCode()))
		}
		fmt.Fprintf(os.Stderr, "error running tests: %v\n", runErr)
		os.Exit(1)
	}
}

// compileTestBinaryWithCoverage compiles a test binary with coverage instrumentation enabled.
// Returns the binary path and the coverage region metadata for report formatting.
func compileTestBinaryWithCoverage(file *ast.File, info *sema.Info, targetTriple, sourceFile string, testTimeouts map[string]int64, testMemoryLimits map[string]int64) (string, []codegen.CoverageRegion) {
	target := targetTriple
	if target == "" {
		target = codegen.HostTargetTriple()
	}
	result := codegen.CompileWithOptions(file, info, target, &codegen.CompileOptions{
		CoverageEnabled:       true,
		DebugAllocator:        true,                    // tests always use debug mode
		MemoryLimitAccounting: testMemoryLimits != nil, // T0689
	})
	result.SetTestMemoryLimits(testMemoryLimits)
	result.GenerateTestMain(info.Tests, testTimeouts)

	ext := binaryExtension(target)
	tmpOutput, err := os.CreateTemp("", "promise-test-cov-*"+ext)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating temp file: %v\n", err)
		os.Exit(1)
	}
	tmpOutput.Close()
	compileAndLink(result, tmpOutput.Name(), target, sourceFile, false) // tests always use debug mode
	return tmpOutput.Name(), result.CoverageRegions
}

// runTestBinaryWithCoverage executes an instrumented test binary, prints test
// results, extracts coverage counter data, and formats a coverage report.
func runTestBinaryWithCoverage(binaryPath string, timeout time.Duration, start time.Time, targetTriple string, regions []codegen.CoverageRegion) {
	target := targetTriple
	if target == "" {
		target = codegen.HostTargetTriple()
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var cmd *exec.Cmd
	switch {
	case isWasmWebTarget(target):
		cmd = runWasmWeb(ctx, binaryPath)
	case isWasmTarget(target):
		cmd = exec.CommandContext(ctx, "wasmtime", binaryPath)
	default:
		cmd = exec.CommandContext(ctx, binaryPath)
	}
	isolateProcessGroup(cmd)
	output, runErr := cmd.CombinedOutput()
	elapsed := time.Since(start)

	if ctx.Err() == context.DeadlineExceeded {
		printTestOutput(string(output))
		fmt.Fprintf(os.Stderr, "TIMEOUT: tests exceeded %s timeout\n", timeout)
		os.Exit(1)
	}

	// Split output into test output and coverage data
	fullOutput := string(output)
	testOutput, counters := extractCoverageData(fullOutput)

	// Print test output (same formatting as runTestBinary)
	summaryRe := regexp.MustCompile(`^(\d+) passed, (\d+) failed(?:, (\d+) skipped)?(?:, (\d+) leaked)?(?:, (\d+) timed out)?(?:, (\d+) allowed leaks)?(?:, (\d+) stale allow_leaks)?`)
	for _, line := range strings.Split(strings.TrimSpace(testOutput), "\n") {
		if line == "" {
			continue
		}
		if m := summaryRe.FindStringSubmatch(line); m != nil {
			fmt.Println()
			summary := fmt.Sprintf("%s passed, %s failed", m[1], m[2])
			if m[3] != "" {
				summary += fmt.Sprintf(", %s skipped", m[3])
			}
			if len(m) > 4 && m[4] != "" {
				summary += fmt.Sprintf(", %s leaked", m[4])
			}
			if len(m) > 5 && m[5] != "" {
				summary += fmt.Sprintf(", %s timed out", m[5])
			}
			if len(m) > 6 && m[6] != "" {
				summary += fmt.Sprintf(", %s allowed leaks", m[6])
			}
			if len(m) > 7 && m[7] != "" {
				summary += fmt.Sprintf(", %s stale allow_leaks", m[7])
			}
			fmt.Printf("%s (%.3fs)\n", summary, elapsed.Seconds())
		} else {
			fmt.Println(line)
		}
	}

	// Print coverage report
	if len(regions) > 0 && len(counters) == len(regions) {
		printCoverageReport(regions, counters)
	}

	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			os.Exit(sanitizeExitCode(exitErr.ExitCode()))
		}
		fmt.Fprintf(os.Stderr, "error running tests: %v\n", runErr)
		os.Exit(1)
	}
}

// extractCoverageData splits binary output into test output and coverage counter values.
// Coverage data is delimited by ===PROMISE_COV=== and ===END_COV=== markers.
func extractCoverageData(output string) (testOutput string, counters []int64) {
	startMarker := "===PROMISE_COV===\n"
	endMarker := "===END_COV===\n"

	startIdx := strings.Index(output, startMarker)
	if startIdx < 0 {
		return output, nil
	}

	testOutput = output[:startIdx]
	covSection := output[startIdx+len(startMarker):]

	endIdx := strings.Index(covSection, endMarker)
	if endIdx < 0 {
		return testOutput, nil
	}
	covSection = covSection[:endIdx]

	for _, line := range strings.Split(strings.TrimSpace(covSection), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		val, err := strconv.ParseInt(line, 10, 64)
		if err != nil {
			continue
		}
		counters = append(counters, val)
	}
	return testOutput, counters
}

// printCoverageReport formats and prints a coverage summary from regions and counters.
func printCoverageReport(regions []codegen.CoverageRegion, counters []int64) {
	fmt.Println()
	fmt.Println("=== Coverage ===")

	// Group regions by file
	type fileStats struct {
		total   int
		covered int
	}
	fileMap := make(map[string]*fileStats)
	var fileOrder []string

	// Also collect per-function stats
	type funcStats struct {
		total   int
		covered int
	}
	funcMap := make(map[string]*funcStats)
	var funcOrder []string

	totalRegions := 0
	coveredRegions := 0

	for i, region := range regions {
		count := counters[i]
		totalRegions++
		if count > 0 {
			coveredRegions++
		}

		// File stats
		fs, ok := fileMap[region.File]
		if !ok {
			fs = &fileStats{}
			fileMap[region.File] = fs
			fileOrder = append(fileOrder, region.File)
		}
		fs.total++
		if count > 0 {
			fs.covered++
		}

		// Function stats (only for function/method entries)
		if region.Kind == "function" || region.Kind == "method" {
			key := region.FuncName
			fns, ok := funcMap[key]
			if !ok {
				fns = &funcStats{}
				funcMap[key] = fns
				funcOrder = append(funcOrder, key)
			}
			fns.total++
			if count > 0 {
				fns.covered++
			}
		}
	}

	// Print per-file summary
	for _, file := range fileOrder {
		fs := fileMap[file]
		pct := 0.0
		if fs.total > 0 {
			pct = float64(fs.covered) / float64(fs.total) * 100
		}
		fmt.Printf("  %-40s %.1f%%\t(%d/%d blocks)\n", file, pct, fs.covered, fs.total)
	}

	// Print per-function detail
	if len(funcOrder) > 0 {
		fmt.Println()
		for _, name := range funcOrder {
			fns := funcMap[name]
			hit := "covered"
			if fns.covered == 0 {
				hit = "not covered"
			}
			fmt.Printf("  %-40s %s\n", name, hit)
		}
	}

	// Print total
	totalPct := 0.0
	if totalRegions > 0 {
		totalPct = float64(coveredRegions) / float64(totalRegions) * 100
	}
	fmt.Printf("\ntotal: %.1f%% (%d/%d blocks)\n", totalPct, coveredRegions, totalRegions)
}

// runE2ETest compiles and runs a .pr file with `test(expected="..."), comparing output.
// executeE2EBinary runs a compiled E2E binary and compares its output.
func executeE2EBinary(binaryPath, expected string, excludeTargets []string,
	filename string, timeout time.Duration, start time.Time, targetTriple string) {

	name := strings.TrimSuffix(filepath.Base(filename), ".pr")

	target := targetTriple
	if target == "" {
		target = codegen.HostTargetTriple()
	}

	targetSuffix := ""
	if targetTriple != "" && targetTriple != codegen.HostTargetTriple() {
		targetSuffix = fmt.Sprintf(" [%s]", targetTriple)
	}

	if isTestExcluded(target, excludeTargets) {
		fmt.Printf("SKIP (excluded) %s%s\n", name, targetSuffix)
		return
	}

	// Execute with timeout, capturing combined stdout+stderr
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var cmd *exec.Cmd
	switch {
	case isWasmWebTarget(target):
		cmd = runWasmWeb(ctx, binaryPath)
	case isWasmTarget(target):
		cmd = exec.CommandContext(ctx, "wasmtime", binaryPath)
	default:
		cmd = exec.CommandContext(ctx, binaryPath)
	}
	isolateProcessGroup(cmd)
	output, err := cmd.CombinedOutput()
	elapsed := time.Since(start)

	if ctx.Err() == context.DeadlineExceeded {
		// T0742: emit TIMEOUT so the multi-file parent classifies as fileTimedOut.
		fmt.Printf("TIMEOUT (%.3fs) %s%s\n", timeout.Seconds(), name, targetSuffix)
		fmt.Printf("  timeout: exceeded %s limit\n", timeout)
		fmt.Printf("\n0 passed, 0 failed, 1 timed out (%.3fs)%s\n", elapsed.Seconds(), targetSuffix)
		fmt.Printf("\nFAILED:\n  %s (timed out after %s)\n", name, timeout)
		os.Exit(1)
	}

	// Compare output — non-zero exit code is NOT a failure (handles panic tests)
	// Strip \r for Windows — Platform.line_separator is \r\n, and source files may
	// have \r\n from git autocrlf, so normalize both sides to \n-only.
	actual := strings.TrimRight(strings.ReplaceAll(string(output), "\r", ""), "\n")
	expectedTrimmed := strings.TrimRight(strings.ReplaceAll(expected, "\r", ""), "\n")

	if actual == expectedTrimmed {
		fmt.Printf("PASS (%.3fs)%s\n", elapsed.Seconds(), targetSuffix)
		fmt.Printf("\n1 passed, 0 failed (%.3fs)%s\n", elapsed.Seconds(), targetSuffix)
	} else {
		fmt.Printf("FAIL (%.3fs)%s\n", elapsed.Seconds(), targetSuffix)
		fmt.Printf("  expected: %s\n", firstLines(expectedTrimmed, 3))
		fmt.Printf("  actual:   %s\n", firstLines(actual, 3))
		if err != nil {
			fmt.Printf("  exit:     %v\n", err)
		}
		fmt.Printf("\n0 passed, 1 failed (%.3fs)%s\n", elapsed.Seconds(), targetSuffix)
		fmt.Printf("\nFAILED:\n  %s\n", name)
		os.Exit(1)
	}
}

// runE2ETest compiles an E2E test binary, saves it to the cache, and runs it.
func runE2ETest(file *ast.File, info *sema.Info, filename string,
	timeout time.Duration, start time.Time, targetTriple string,
	cacheDir, cacheKey string, compileStart time.Time) {

	target := targetTriple
	if target == "" {
		target = codegen.HostTargetTriple()
	}

	// Check target exclusion before compiling
	if isTestExcluded(target, info.ExcludeTargets) {
		name := strings.TrimSuffix(filepath.Base(filename), ".pr")
		targetSuffix := ""
		if targetTriple != "" && targetTriple != codegen.HostTargetTriple() {
			targetSuffix = fmt.Sprintf(" [%s]", targetTriple)
		}
		fmt.Printf("SKIP (excluded) %s%s\n", name, targetSuffix)
		return
	}

	// Codegen with normal main (no GenerateTestMain)
	tCodegen := time.Now()
	result := codegen.CompileWithOptions(file, info, target, &codegen.CompileOptions{
		DebugAllocator: true, // tests always use debug mode
	})
	timePhase("codegen", time.Since(tCodegen), "")

	ext := binaryExtension(target)
	tmpOutput, err := os.CreateTemp("", "promise-e2e-*"+ext)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating temp file: %v\n", err)
		os.Exit(1)
	}
	tmpOutput.Close()
	defer os.Remove(tmpOutput.Name())

	compileAndLink(result, tmpOutput.Name(), target, filename, false) // tests always use debug mode

	if timePhases && !compileStart.IsZero() {
		timePhase("total", time.Since(compileStart), "")
	}

	// Save to cache
	if cacheDir != "" {
		module.SaveTestBinaryCache(cacheDir, cacheKey, tmpOutput.Name())
		module.SaveTestBinaryMeta(cacheDir, cacheKey, &module.CacheMeta{
			Kind:             module.CacheKindBinary,
			Name:             filename,
			CacheKey:         cacheKey,
			E2E:              true,
			ExpectedOutput:   info.ExpectOutput,
			ExcludeTargets:   info.ExcludeTargets,
			ProcessTimeoutNs: (timeout + 30*time.Second).Nanoseconds(),
		})
	}

	executeE2EBinary(tmpOutput.Name(), info.ExpectOutput, info.ExcludeTargets,
		filename, timeout, start, targetTriple)
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

// buildChildTestArgs constructs the argument list (excluding the trailing test
// file path) for a child `promise test` process spawned by runTestFiles. It is
// extracted from the inline construction so the flag-forwarding logic — notably
// the T0738 -memory-limit forwarding — is unit-testable.
func buildChildTestArgs(cfg testTimeoutConfig, targetTriple string, coverageMode, timePhases bool) []string {
	testArgs := []string{"test", "-timeout", fmt.Sprintf("%gs", cfg.defaultTimeout.Seconds())}
	if cfg.scale != 1.0 {
		testArgs = append(testArgs, "-timeout-scale", fmt.Sprintf("%g", cfg.scale))
	}
	if cfg.min > 0 {
		testArgs = append(testArgs, "-timeout-min", cfg.min.String())
	}
	if cfg.max > 0 {
		testArgs = append(testArgs, "-timeout-max", cfg.max.String())
	}
	if targetTriple != "" {
		testArgs = append(testArgs, "-target", targetTriple)
	}
	if coverageMode {
		testArgs = append(testArgs, "-coverage")
	}
	if timePhases {
		testArgs = append(testArgs, "-time-phases")
	}
	if jsonMode {
		// Children emit a roster marker but run the single-file path — they must
		// NOT receive --json, which would make them re-enter the fan-out (a fork
		// bomb). The parent owns JSONL emission. T0763.
		testArgs = append(testArgs, "-child-roster")
	}
	if cfg.compileTimeout != 10*time.Minute {
		testArgs = append(testArgs, "-compile-timeout", cfg.compileTimeout.String())
	}
	// T0738: forward -memory-limit only when it was explicitly set on the
	// parent CLI. Without this, every child re-parses its own args, sees no
	// -memory-limit, and applies its own 2 GiB default — silently dropping an
	// explicit tighten/raise/0-opt-out in multi-file runs. %dB round-trips
	// byte-exact through the child's parseMemoryLimitArg (the opt-out forwards
	// as "0B" → 0). When not explicit, omit it so each child applies its own
	// default (which equals the parent's default).
	if cfg.memoryLimitExplicit {
		testArgs = append(testArgs, "-memory-limit", fmt.Sprintf("%dB", cfg.defaultMemoryBytes))
	}
	return testArgs
}

// passingTest is a single passing batch test recovered from a child test
// process's stdout (T0749).
type passingTest struct {
	name    string
	elapsed float64
}

// reportTestRecord is one entry in the -report-json sidecar: a passing batch
// test identified by (file, test). Snapshot/E2E tests have no test-function
// name and are intentionally absent (identified by file alone). T0749.
type reportTestRecord struct {
	File    string  `json:"file"`
	Test    string  `json:"test"`
	Elapsed float64 `json:"elapsed"`
}

// testReport is the envelope written by -report-json. The schema may grow;
// "passing" lists per-(file, test) records for passing batch tests. T0749.
type testReport struct {
	Passing []reportTestRecord `json:"passing"`
}

// passingTestLineRe matches a passing batch-test line from a single-file child's
// output, e.g. "pass (0.009s) test_and" (host) or "pass (0.009s) test_and
// [wasm32-wasi]" (cross-target). A test name is a single whitespace-free
// identifier, so capturing one non-space token naturally drops any trailing
// " [target]" suffix. Snapshot/E2E files print uppercase "PASS (Xs)" with no
// name and do not match.
var passingTestLineRe = regexp.MustCompile(`^pass \(([\d.]+)s\) (\S+)`)

// extractPassingTestNames returns the passing batch tests recorded in a
// single-file child's captured output. Lines whose name contains ".pr" (e.g. an
// aggregated file-level line) are skipped defensively. T0749.
func extractPassingTestNames(childOutput string) []passingTest {
	var out []passingTest
	for _, line := range strings.Split(childOutput, "\n") {
		m := passingTestLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[2]
		if name == "" || strings.Contains(name, ".pr") {
			continue
		}
		elapsed, _ := strconv.ParseFloat(m[1], 64)
		out = append(out, passingTest{name: name, elapsed: elapsed})
	}
	return out
}

// writeTestReport writes the passing-test report to path. Best-effort: a marshal
// or write error warns on stderr and never fails the test run. T0749.
func writeTestReport(path string, records []reportTestRecord) {
	data, err := json.MarshalIndent(testReport{Passing: records}, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot marshal test report: %v\n", err)
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot write test report %s: %v\n", path, err)
	}
}

// runTestFiles runs tests from a list of .pr files, printing per-file results
// and a combined summary at the end. Tests are compiled and run concurrently
// up to the parallel limit. Results are printed in file order.
//
// T0213: Batch vs individual test run differences. Each file runs as a separate
// subprocess (process-isolated), so tests don't share global state or memory
// layout. Remaining differences after T0213 fixes:
//   - Process timeout: batch uses computeParentTimeout (10min backstop);
//     individual uses sum-of-per-test-timeouts + 30s. Both are safety nets only.
//   - Parallelism: batch runs multiple files concurrently (resource contention
//     possible under heavy load); individual runs one file at a time.
func runTestFiles(files []string, cfg testTimeoutConfig, targetTriple string, parallel int, coverageMode bool, reportJSON string) {
	unlock := module.LockBuildDirShared()
	defer unlock()

	// Ensure embedded module extraction completes before spawning child
	// processes. Each child calls extractEmbeddedModule independently; if
	// the cache is empty (first run or after compiler change), concurrent
	// children race on directory creation + file writes. Extracting all
	// modules here in the parent ensures the cache is populated first.
	ensureCacheValid()
	if entries, err := embeddedModules.ReadDir("resources/modules"); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				if _, err := extractEmbeddedModule(e.Name()); err != nil {
					fmt.Fprintf(os.Stderr, "error extracting module %s: %v\n", e.Name(), err)
					os.Exit(1)
				}
			}
		}
	}

	totalStart := time.Now()

	if len(files) == 0 {
		fmt.Println("no test files found")
		return
	}

	selfExe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot determine executable path: %v\n", err)
		os.Exit(1)
	}

	// Find common base directory for relative path display.
	baseDir := commonDir(files)

	targetSuffix := ""
	if targetTriple != "" && targetTriple != codegen.HostTargetTriple() {
		targetSuffix = fmt.Sprintf(" [%s]", targetTriple)
	}

	// Dedup module test files: all *_test.pr files in the same module compile
	// together into one binary, so we only need to run one per module.
	moduleTestSeen := map[string]bool{}
	var dedupedFiles []string
	for _, f := range files {
		if modDir := isModuleTestFile(f); modDir != "" {
			if moduleTestSeen[modDir] {
				continue
			}
			moduleTestSeen[modDir] = true
		}
		dedupedFiles = append(dedupedFiles, f)
	}

	type fileResult struct {
		file     string
		output   string
		cmdErr   error
		timedOut bool
		elapsed  time.Duration
		done     chan struct{} // closed when result is ready
	}

	// Run all tests concurrently with a semaphore.
	results := make([]fileResult, len(dedupedFiles))
	for i, f := range dedupedFiles {
		results[i].file = f
		results[i].done = make(chan struct{})
	}
	sem := make(chan struct{}, parallel)

	for i := range dedupedFiles {
		go func(idx int) {
			sem <- struct{}{}        // acquire
			defer func() { <-sem }() // release

			r := &results[idx]
			fileStart := time.Now()
			// The parent context is a backstop for hung compilations, not
			// a test timeout. Per-test timeouts are handled by the subprocess.
			parentTimeout := computeParentTimeout(cfg, targetTriple)
			ctx, cancel := context.WithTimeout(context.Background(), parentTimeout)
			defer cancel()
			testArgs := buildChildTestArgs(cfg, targetTriple, coverageMode, timePhases)
			testArgs = append(testArgs, r.file)
			cmd := exec.CommandContext(ctx, selfExe, testArgs...)
			setupProcessGroupKill(cmd)
			output, cmdErr := cmd.CombinedOutput()

			r.output = strings.TrimSpace(string(output))
			r.cmdErr = cmdErr
			r.timedOut = ctx.Err() == context.DeadlineExceeded
			r.elapsed = time.Since(fileStart)
			close(r.done)
		}(i)
	}

	// T0763: JSON mode — emit one newline-delimited record per eligible test to
	// stdout (the gate's authoritative source), bypassing the human aggregation
	// below. A brief summary still goes to stderr.
	if jsonMode {
		counts := map[string]int{}
		for i := range results {
			<-results[i].done
			r := &results[i]
			recs := buildTestRecords(absPath(r.file), r.output, r.cmdErr != nil || r.timedOut)
			writeTestRecords(os.Stdout, recs)
			for _, rec := range recs {
				counts[rec.Status]++
			}
		}
		fmt.Fprintf(os.Stderr, "json: %d pass, %d fail, %d timeout, %d leak, %d memory, %d excluded, %d not-run (%d files, %.3fs)\n",
			counts["pass"], counts["fail"], counts["timeout"], counts["leak"], counts["memory"],
			counts["excluded"], counts["not-run"], len(results), time.Since(totalStart).Seconds())
		return
	}

	// Print results in file order, streaming as each slot completes.
	summaryRe := regexp.MustCompile(`^(\d+) passed, (\d+) failed(?:, (\d+) skipped)?(?:, (\d+) leaked)?(?:, (\d+) timed out)?(?:, (\d+) allowed leaks)?(?:, (\d+) stale allow_leaks)?(?:, (\d+) memlimit)?`)
	failLineRe := regexp.MustCompile(`^FAIL \([\d.]+s\)(?: (.+))?$`)
	leakLineRe := regexp.MustCompile(`^LEAK \([\d.]+s\)(?: (.+))?$`)
	timeoutLineRe := regexp.MustCompile(`^TIMEOUT \([\d.]+s\)(?: (.+))?$`)
	memlimitLineRe := regexp.MustCompile(`^MEMLIMIT \([^)]+\)(?: (.+))?$`) // T0689
	passLineRe := regexp.MustCompile(`^pass \([\d.]+s\)`)
	panicContextRe := regexp.MustCompile(`^  (panic:|expected:|actual:|exit:|leak:|warning:|fatal:|signal:|timeout:|memory limit:)`)

	type failureInfo struct {
		name    string
		context string
	}

	totalPassed := 0
	totalFailed := 0
	totalSkipped := 0
	totalLeaked := 0
	totalTimedOut := 0
	totalMemlimited := 0 // T0689
	totalIgnored := 0
	totalStale := 0
	totalFiles := 0
	failedFiles := 0
	var failures []failureInfo
	var staleTests []string
	var reportRecords []reportTestRecord // T0749: passing batch tests for -report-json

	// Coverage aggregation: collect per-file stats from subprocess formatted output.
	// Each subprocess prints "total: X% (Y/Z blocks)" which we parse.
	type fileCoverage struct {
		file    string
		covered int
		total   int
	}
	var covFiles []fileCoverage
	covTotalRe := regexp.MustCompile(`^total: [\d.]+% \((\d+)/(\d+) blocks\)`)

	for i := range results {
		<-results[i].done
		r := &results[i]

		// Extract coverage summary from subprocess output and strip coverage lines
		if coverageMode {
			var cleanLines []string
			inCovSection := false
			for _, line := range strings.Split(r.output, "\n") {
				if line == "=== Coverage ===" {
					inCovSection = true
					continue
				}
				if inCovSection {
					if m := covTotalRe.FindStringSubmatch(line); m != nil {
						covFiles = append(covFiles, fileCoverage{
							file:    r.file,
							covered: atoi(m[1]),
							total:   atoi(m[2]),
						})
					}
					continue // skip all coverage lines
				}
				cleanLines = append(cleanLines, line)
			}
			r.output = strings.Join(cleanLines, "\n")
		}

		// Skip files with no tests or excluded for this target
		last := lastLine(r.output)
		if !r.timedOut && (last == "no tests found" || strings.HasPrefix(last, "SKIP")) {
			if strings.HasPrefix(last, "SKIP") {
				totalSkipped++
			}
			continue
		}

		totalFiles++

		relPath, relErr := filepath.Rel(baseDir, r.file)
		if relErr != nil {
			relPath = r.file
		}

		if r.timedOut {
			fmt.Printf("FAIL (%.3fs) %s (compilation timeout)%s\n", r.elapsed.Seconds(), relPath, targetSuffix)
			failedFiles++
			totalFailed++
			failures = append(failures, failureInfo{name: relPath + " (compilation timeout)"})
			continue
		}

		// Parse the subprocess output into structured results
		lines := strings.Split(r.output, "\n")
		filePassed := 0
		fileFailed := 0
		var failDetails []string

		fileLeaked := 0
		fileTimedOut := 0
		fileMemlimited := 0 // T0689
		var summaryMatch []string
		for i := 0; i < len(lines); i++ {
			line := lines[i]
			if passLineRe.MatchString(line) {
				filePassed++
			} else if m := failLineRe.FindStringSubmatch(line); m != nil {
				fileFailed++
				detail := m[1]
				for i+1 < len(lines) && panicContextRe.MatchString(lines[i+1]) {
					i++
					if detail == "" {
						detail = strings.TrimSpace(lines[i])
					} else {
						detail += "\n" + lines[i]
					}
				}
				if detail != "" {
					failDetails = append(failDetails, detail)
				}
			} else if m := leakLineRe.FindStringSubmatch(line); m != nil {
				fileLeaked++
				detail := m[1]
				for i+1 < len(lines) && panicContextRe.MatchString(lines[i+1]) {
					i++
					if detail == "" {
						detail = strings.TrimSpace(lines[i])
					} else {
						detail += "\n" + lines[i]
					}
				}
				if detail != "" {
					failDetails = append(failDetails, detail)
				}
			} else if m := timeoutLineRe.FindStringSubmatch(line); m != nil {
				fileTimedOut++
				detail := m[1]
				for i+1 < len(lines) && panicContextRe.MatchString(lines[i+1]) {
					i++
					if detail == "" {
						detail = strings.TrimSpace(lines[i])
					} else {
						detail += "\n" + lines[i]
					}
				}
				if detail != "" {
					failDetails = append(failDetails, detail)
				}
			} else if m := memlimitLineRe.FindStringSubmatch(line); m != nil {
				// T0689: MEMLIMIT outcome (synthetic) — the test process aborted.
				fileMemlimited++
				detail := m[1]
				for i+1 < len(lines) && panicContextRe.MatchString(lines[i+1]) {
					i++
					if detail == "" {
						detail = strings.TrimSpace(lines[i])
					} else {
						detail += "\n" + lines[i]
					}
				}
				if detail != "" {
					failDetails = append(failDetails, detail)
				}
			} else if sm := summaryRe.FindStringSubmatch(line); sm != nil {
				summaryMatch = sm
			}
		}

		// T0749: record passing batch-test names so the gate's -report-json
		// sidecar can give each passing test a stable (file, test) identity.
		// Snapshot/E2E files print uppercase "PASS" with no name and contribute
		// nothing here, so they remain file-identified. relPath matches the file
		// key the gate parses from the compact pass line printed below.
		if reportJSON != "" {
			for _, pt := range extractPassingTestNames(r.output) {
				reportRecords = append(reportRecords, reportTestRecord{
					File:    relPath,
					Test:    pt.name,
					Elapsed: pt.elapsed,
				})
			}
		}

		if r.cmdErr != nil {
			// Test binaries may crash during scheduler shutdown after all tests pass
			// (stack overflow on macOS/Linux, STATUS_ACCESS_VIOLATION on Windows).
			// If all tests passed, none failed, AND the summary line was printed
			// (meaning the test harness completed), treat as a pass — the crash is
			// in the shutdown path, not in user code. B0230.
			// Without a summary line, the subprocess crashed mid-test (B0300).
			if filePassed > 0 && fileFailed == 0 && fileLeaked == 0 && fileTimedOut == 0 && fileMemlimited == 0 && summaryMatch != nil {
				relPath, relErr := filepath.Rel(baseDir, r.file)
				if relErr != nil {
					relPath = r.file
				}
				m := summaryMatch
				totalPassed += atoi(m[1])
				totalFailed += atoi(m[2])
				if m[3] != "" {
					totalSkipped += atoi(m[3])
				}
				if len(m) > 4 && m[4] != "" {
					totalLeaked += atoi(m[4])
				}
				if len(m) > 5 && m[5] != "" {
					totalTimedOut += atoi(m[5])
				}
				if len(m) > 6 && m[6] != "" {
					totalIgnored += atoi(m[6])
				}
				if len(m) > 7 && m[7] != "" {
					totalStale += atoi(m[7])
				}
				if len(m) > 8 && m[8] != "" {
					totalMemlimited += atoi(m[8])
				}
				totalFiles++
				testCount := ""
				if filePassed > 1 {
					testCount = fmt.Sprintf(" (%d tests)", filePassed)
				}
				fmt.Printf("pass (%.3fs) %s%s%s\n", r.elapsed.Seconds(), relPath, testCount, targetSuffix)
				continue
			}
			failedFiles++

			// B0300: subprocess crashed before printing summary — report as crash.
			if summaryMatch == nil && filePassed > 0 && fileFailed == 0 && fileLeaked == 0 && fileTimedOut == 0 {
				totalPassed += filePassed
				totalFailed++
				errCtx := r.cmdErr.Error()
				fmt.Printf("FAIL (%.3fs) %s (crashed after %d tests)%s\n", r.elapsed.Seconds(), relPath, filePassed, targetSuffix)
				fmt.Printf("  process crashed: %s\n", errCtx)
				failures = append(failures, failureInfo{name: relPath + " (crashed)", context: errCtx})
				continue
			}

			if m := summaryMatch; m != nil {
				totalPassed += atoi(m[1])
				totalFailed += atoi(m[2])
				if m[3] != "" {
					totalSkipped += atoi(m[3])
				}
				if len(m) > 4 && m[4] != "" {
					totalLeaked += atoi(m[4])
					fileLeaked = atoi(m[4])
				}
				if len(m) > 5 && m[5] != "" {
					totalTimedOut += atoi(m[5])
					fileTimedOut = atoi(m[5])
				}
				if len(m) > 6 && m[6] != "" {
					totalIgnored += atoi(m[6])
				}
				if len(m) > 7 && m[7] != "" {
					totalStale += atoi(m[7])
				}
				if len(m) > 8 && m[8] != "" {
					totalMemlimited += atoi(m[8])
					fileMemlimited = atoi(m[8])
				}
			} else if fileFailed > 0 || filePassed > 0 {
				totalPassed += filePassed
				totalFailed += fileFailed
			} else {
				totalFailed++
				fmt.Printf("FAIL (%.3fs) %s (compilation error)%s\n", r.elapsed.Seconds(), relPath, targetSuffix)
				var errCtx string
				for _, line := range lines {
					if line != "" && !summaryRe.MatchString(line) {
						errCtx = line
						fmt.Printf("  %s\n", line)
						break
					}
				}
				failures = append(failures, failureInfo{name: relPath + " (compilation error)", context: errCtx})
				continue
			}

			totalTests := filePassed + fileFailed + fileLeaked + fileTimedOut + fileMemlimited
			if fileFailed == 0 && fileLeaked > 0 && fileTimedOut == 0 && fileMemlimited == 0 {
				fmt.Printf("FAIL (%.3fs) %s (%d leaked)%s\n", r.elapsed.Seconds(), relPath, fileLeaked, targetSuffix)
			} else if fileFailed == 0 && fileTimedOut > 0 && fileLeaked == 0 && fileMemlimited == 0 {
				fmt.Printf("FAIL (%.3fs) %s (%d timed out)%s\n", r.elapsed.Seconds(), relPath, fileTimedOut, targetSuffix)
			} else if fileFailed == 0 && fileMemlimited > 0 && fileLeaked == 0 && fileTimedOut == 0 {
				// T0689: pure memlimit abort — process didn't run all tests.
				fmt.Printf("FAIL (%.3fs) %s (memory limit exceeded)%s\n", r.elapsed.Seconds(), relPath, targetSuffix)
			} else if totalTests > 0 {
				failCount := fileFailed + fileLeaked + fileTimedOut + fileMemlimited
				fmt.Printf("FAIL (%.3fs) %s (%d/%d failed)%s\n", r.elapsed.Seconds(), relPath, failCount, totalTests, targetSuffix)
			} else {
				fmt.Printf("FAIL (%.3fs) %s%s\n", r.elapsed.Seconds(), relPath, targetSuffix)
			}
			for _, detail := range failDetails {
				parts := strings.SplitN(detail, "\n", 2)
				testName := parts[0]
				var panicCtx string
				for _, dl := range strings.Split(detail, "\n") {
					fmt.Printf("  %s\n", dl)
				}
				if len(parts) > 1 {
					panicCtx = strings.TrimPrefix(parts[1], "  ")
				}
				failures = append(failures, failureInfo{name: relPath + ": " + testName, context: panicCtx})
			}
			if len(failDetails) == 0 {
				// No individual test failures — extract context from output
				// (e.g., leak-only failures, runtime crashes)
				var errCtx string
				for _, line := range lines {
					trimmed := strings.TrimSpace(line)
					if trimmed == "" || summaryRe.MatchString(line) || passLineRe.MatchString(line) || failLineRe.MatchString(line) || leakLineRe.MatchString(line) || timeoutLineRe.MatchString(line) {
						continue
					}
					errCtx = trimmed
					fmt.Printf("  %s\n", trimmed)
					break
				}
				if errCtx == "" && r.cmdErr != nil {
					errCtx = r.cmdErr.Error()
				}
				failures = append(failures, failureInfo{name: relPath, context: errCtx})
			}
			continue
		}

		// Success
		if m := summaryMatch; m != nil {
			totalPassed += atoi(m[1])
			totalFailed += atoi(m[2])
			if m[3] != "" {
				totalSkipped += atoi(m[3])
			}
			if len(m) > 4 && m[4] != "" {
				totalLeaked += atoi(m[4])
			}
			if len(m) > 5 && m[5] != "" {
				totalTimedOut += atoi(m[5])
			}
			if len(m) > 6 && m[6] != "" {
				totalIgnored += atoi(m[6])
			}
			if len(m) > 7 && m[7] != "" {
				totalStale += atoi(m[7])
			}
			if len(m) > 8 && m[8] != "" {
				totalMemlimited += atoi(m[8])
			}
		}

		// Parse stale allow_leaks tests from output
		for j := 0; j < len(lines); j++ {
			if lines[j] == "STALE ALLOW_LEAKS:" {
				for j+1 < len(lines) && strings.HasPrefix(lines[j+1], "  ") {
					j++
					staleTests = append(staleTests, relPath+": "+strings.TrimSpace(lines[j]))
				}
			}
		}

		totalTests := filePassed + fileFailed
		if totalTests > 1 {
			fmt.Printf("pass (%.3fs) %s (%d tests)%s\n", r.elapsed.Seconds(), relPath, totalTests, targetSuffix)
		} else {
			fmt.Printf("pass (%.3fs) %s%s\n", r.elapsed.Seconds(), relPath, targetSuffix)
		}
	}

	// T0749: emit the machine-readable passing-test report (parent-only) before
	// any exit path so the gate can correlate test identity by (file, test).
	if reportJSON != "" {
		writeTestReport(reportJSON, reportRecords)
	}

	if totalFiles == 0 {
		fmt.Println("no test files found")
		return
	}

	// Print grand summary
	fmt.Println()
	totalElapsed := time.Since(totalStart)
	summary := fmt.Sprintf("%d passed, %d failed", totalPassed, totalFailed)
	if totalSkipped > 0 {
		summary += fmt.Sprintf(", %d skipped", totalSkipped)
	}
	if totalLeaked > 0 {
		summary += fmt.Sprintf(", %d leaked", totalLeaked)
	}
	if totalTimedOut > 0 {
		summary += fmt.Sprintf(", %d timed out", totalTimedOut)
	}
	if totalMemlimited > 0 {
		summary += fmt.Sprintf(", %d memlimit", totalMemlimited)
	}
	if totalIgnored > 0 {
		summary += fmt.Sprintf(", %d allowed leaks", totalIgnored)
	}
	if totalStale > 0 {
		summary += fmt.Sprintf(", %d stale allow_leaks", totalStale)
	}
	fmt.Printf("%s (%d files, %.3fs)%s\n", summary, totalFiles, totalElapsed.Seconds(), targetSuffix)

	if len(failures) > 0 {
		fmt.Printf("\nFAILED:\n")
		for _, f := range failures {
			fmt.Printf("  %s\n", f.name)
			if f.context != "" {
				for _, cl := range strings.Split(f.context, "\n") {
					fmt.Printf("    %s\n", cl)
				}
			}
		}
	}

	if len(staleTests) > 0 {
		fmt.Printf("\nSTALE ALLOW_LEAKS:\n")
		for _, s := range staleTests {
			fmt.Printf("  %s\n", s)
		}
	}

	// Print aggregated coverage report for multi-file coverage mode
	if coverageMode && len(covFiles) > 0 {
		fmt.Println()
		fmt.Println("=== Coverage ===")
		totalCovered := 0
		totalBlocks := 0
		for _, cf := range covFiles {
			totalCovered += cf.covered
			totalBlocks += cf.total
			pct := 0.0
			if cf.total > 0 {
				pct = float64(cf.covered) / float64(cf.total) * 100
			}
			relPath, relErr := filepath.Rel(baseDir, cf.file)
			if relErr != nil {
				relPath = cf.file
			}
			fmt.Printf("  %-50s %.1f%%\t(%d/%d blocks)\n", relPath, pct, cf.covered, cf.total)
		}
		totalPct := 0.0
		if totalBlocks > 0 {
			totalPct = float64(totalCovered) / float64(totalBlocks) * 100
		}
		fmt.Printf("\ntotal: %.1f%% (%d/%d blocks)\n", totalPct, totalCovered, totalBlocks)
	}

	// T0109: Leak-only failures must also produce non-zero exit code.
	// The B0230 workaround (line ~1466) treats crash-during-shutdown as PASS
	// when filePassed > 0 && fileFailed == 0, but this also swallows leak-only
	// exits. Rather than changing that logic, enforce leaks at the final gate.
	if totalFailed > 0 || totalLeaked > 0 || totalTimedOut > 0 || totalMemlimited > 0 || failedFiles > 0 {
		os.Exit(1)
	}
}

// dirHasTestFiles checks if a directory contains any test .pr files (non-recursive).
// dirHasTestFiles reports whether dir contains any *_test.pr files.
func dirHasTestFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), "_test.pr") {
			return true
		}
	}
	return false
}

// discoverTestFiles finds test .pr files in a directory.
// In module directories (containing promise.toml), only *_test.pr files are returned.
// In non-module directories, all .pr files are returned (they're standalone tests).
func discoverTestFiles(dir string, recursive bool) []string {
	var files []string

	// isModuleDir caches whether a directory is a module source directory.
	modDirCache := map[string]bool{}
	isModuleDir := func(d string) bool {
		if v, ok := modDirCache[d]; ok {
			return v
		}
		_, err := os.Stat(filepath.Join(d, "promise.toml"))
		v := err == nil
		modDirCache[d] = v
		return v
	}

	// isTestFile returns true if the file name matches the *_test.pr convention.
	isTestFile := func(name string) bool {
		return strings.HasSuffix(name, "_test.pr")
	}

	// isInModuleTree checks if a file is inside a module directory tree
	// (any ancestor up to the walk root has promise.toml, but no closer
	// promise.toml in between that would make it a nested module).
	isInModuleTree := func(filePath string) bool {
		d := filepath.Dir(filePath)
		for d != dir {
			if isModuleDir(d) {
				return true
			}
			parent := filepath.Dir(d)
			if parent == d {
				break
			}
			d = parent
		}
		return isModuleDir(dir)
	}

	if recursive {
		if err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			// Skip module directories that have no test files at all.
			if d.IsDir() && path != dir {
				if isModuleDir(path) && !dirHasTestFiles(path) {
					return filepath.SkipDir
				}
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(d.Name(), ".pr") {
				return nil
			}
			// In module directory trees, only pick up test files (not implementation).
			// This handles both direct module dirs and their subdirs.
			if isInModuleTree(path) && !isTestFile(d.Name()) {
				return nil
			}
			files = append(files, path)
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
		moduleDir := isModuleDir(dir)
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".pr") {
				continue
			}
			// In module directories, only pick up test files.
			if moduleDir && !isTestFile(e.Name()) {
				continue
			}
			files = append(files, filepath.Join(dir, e.Name()))
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

// computeParentTimeout returns the backstop timeout for a subprocess that compiles
// and runs a single test file. This is a safety net for hung compilations (opt/llc
// stuck forever), NOT a test timeout — per-test timeouts handle slow tests. The
// default is 10 minutes (-compile-timeout flag). WASM cross-compilation uses a
// higher minimum (15 min) since it's significantly slower (B0108).
func computeParentTimeout(cfg testTimeoutConfig, target string) time.Duration {
	backstop := cfg.compileTimeout
	if backstop == 0 {
		backstop = 10 * time.Minute
	}
	if isWasmTarget(target) && backstop < 15*time.Minute {
		backstop = 15 * time.Minute
	}
	return backstop
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

// parseMemoryLimitArg parses a memory-limit string. Requires an explicit unit
// suffix (B/KB/MB/GB or KiB/MiB/GiB, case-insensitive). "0" without a unit is a
// special opt-out value. Negative numbers and bare numbers (non-zero) are
// rejected to prevent ambiguity (T0689).
//
// Returns bytes as int64. Allowed grammar (regex-ish):
//
//	"0"                       → 0 (opt-out)
//	N + ("b" | "kb" | "mb" | "gb")     → binary multipliers (1024)
//	N + ("kib" | "mib" | "gib")        → binary multipliers (1024)
//
// We use binary multipliers for KB/MB/GB by convention (matches user
// expectations for "256MB" = 256 * 1024^2 bytes, consistent with how memory
// is colloquially counted).
func parseMemoryLimitArg(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("invalid memory limit: empty string")
	}
	if s == "0" {
		return 0, nil
	}
	lower := strings.ToLower(s)
	var mult int64
	var numPart string
	switch {
	case strings.HasSuffix(lower, "gib"):
		mult, numPart = 1<<30, strings.TrimSuffix(lower, "gib")
	case strings.HasSuffix(lower, "mib"):
		mult, numPart = 1<<20, strings.TrimSuffix(lower, "mib")
	case strings.HasSuffix(lower, "kib"):
		mult, numPart = 1<<10, strings.TrimSuffix(lower, "kib")
	case strings.HasSuffix(lower, "gb"):
		mult, numPart = 1<<30, strings.TrimSuffix(lower, "gb")
	case strings.HasSuffix(lower, "mb"):
		mult, numPart = 1<<20, strings.TrimSuffix(lower, "mb")
	case strings.HasSuffix(lower, "kb"):
		mult, numPart = 1<<10, strings.TrimSuffix(lower, "kb")
	case strings.HasSuffix(lower, "b"):
		mult, numPart = 1, strings.TrimSuffix(lower, "b")
	default:
		return 0, fmt.Errorf("invalid memory limit: %q (require unit B/KB/MB/GB, e.g. '256MB' or '2GB'; or '0' to disable)", s)
	}
	numPart = strings.TrimSpace(numPart)
	n, err := strconv.ParseInt(numPart, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid memory limit: %q (numeric part must be a non-negative integer)", s)
	}
	if n > (1<<62)/mult {
		return 0, fmt.Errorf("invalid memory limit: %q (overflows int64)", s)
	}
	return n * mult, nil
}

// compileAndLink writes the IR to a temp file and links it into the output binary.
// On Linux, macOS, and WASM, uses opt + llc + linker pipeline (Phase 7a/7b/7c).
// On other platforms (or with PROMISE_USE_CLANG=1), uses clang as driver.
func compileAndLink(result *codegen.CompileResult, outputFile, target, sourceFile string, releaseMode bool) {
	// Dump generated LLVM IR to a file for debugging/inspection.
	// Usage: PROMISE_DUMP_IR=/tmp/out.ll promise build foo.pr
	if envDump := os.Getenv("PROMISE_DUMP_IR"); envDump != "" {
		_ = os.WriteFile(envDump, []byte(result.Module.String()), 0644)
	}

	// Separate compilation: split IR into main + per-module .ll files
	if result.HasModules() && !useClangPipeline(target) {
		compileAndLinkSeparate(result, outputFile, target, sourceFile, releaseMode)
		return
	}

	// Single-file compilation (no modules or clang fallback)
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

	if useClangPipeline(target) {
		compileAndLinkClang(llFile.Name(), target, outputFile)
	} else {
		compileAndLinkLLVM(llFile.Name(), target, outputFile, releaseMode)
	}
}

// compileAndLinkSeparate compiles each module to its own .bc/.o file, then links
// them together. Release mode uses LTO (--lto-O1); debug mode uses the object
// pipeline (llc → .o → linker with --gc-sections / -dead_strip). Windows uses
// opt+llc object pipeline (no LTO). Uses content-hash caching: if a module's
// source hasn't changed, its cached output is reused.
func compileAndLinkSeparate(result *codegen.CompileResult, outputFile, target, sourceFile string, releaseMode bool) {
	mainIR, moduleIRs := result.SplitModuleIRs()

	optPath, err := findLLVMTool("opt")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	checkLLVMToolVersion(optPath)

	// Release: LTO pipeline (opt -O1 → .bc → linker --lto-O1).
	// Debug: object pipeline (opt -O1 → .bc → llc → .o → linker --gc-sections).
	// WASM: always LTO (--lto-O1 — see T0333; --lto-O2 miscompiles `icmp samesign`).
	// Windows: always object pipeline (LTO not wired up for MSVC).
	// Note: opt -O1 in both modes until B0314 (alloca domination) is fixed to enable -O0.
	useLTO := (releaseMode || isWasmTarget(target)) && !isWindowsTarget(target)
	optLevel := "-O1"

	// Debug mode and Windows need llc for the object pipeline.
	var llcPath string
	if !useLTO {
		llcPath, err = findLLVMTool("llc")
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		checkLLVMToolVersion(llcPath)
	}

	// Build cache (~/.promise/cache/build/, overridable via PROMISE_HOME)
	cacheDir, _ := module.BuildCacheDir()

	compilerHash := module.CompilerHash()
	modInfos := result.ModuleInfos()

	// compileModule compiles one IR text to bitcode (LTO) or object (debug/Windows).
	compileModule := func(irText, prefix string) string {
		if useLTO {
			return compileLLToBC(irText, prefix, optPath, optLevel)
		}
		return compileLLToObj(irText, prefix, target, optPath, llcPath, optLevel)
	}

	// Compile main IR (always recompiled — main changes with every build).
	tOpt := time.Now()
	mainObj := compileModule(mainIR, "promise-main")
	defer os.Remove(mainObj)

	// Compile each module IR in parallel, with caching.
	type moduleObj struct {
		name    string
		objFile string
		cached  bool // if true, objFile points to cache — don't delete
	}
	var moduleObjs []moduleObj

	var wg sync.WaitGroup
	var mu sync.Mutex
	for modName, modIR := range moduleIRs {
		wg.Add(1)
		go func(name, irText string) {
			defer wg.Done()

			// B0244: Module IR can vary across compilation contexts due to cross-module
			// enum clone/drop forward-declarations (e.g., Map[K,V] methods reference
			// __mod_json_JsonValue.clone in cross-module builds but JsonValue.clone in
			// module-internal tests). Hash the IR text to capture these differences.
			contentCacheKey := ""
			if cacheDir != "" {
				h := fnv.New128a()
				h.Write([]byte(irText))
				// Include build mode so debug (.o) and release (.bc) get separate entries.
				if useLTO {
					fmt.Fprint(h, "\nmode:release")
				} else {
					fmt.Fprint(h, "\nmode:debug")
				}
				fmt.Fprintf(h, "\ntarget:%s", target)
				contentCacheKey = hex.EncodeToString(h.Sum(nil))
			}

			// Try cache lookup
			if contentCacheKey != "" {
				if cachedObj := module.LookupBuildCache(cacheDir, contentCacheKey); cachedObj != "" {
					mu.Lock()
					moduleObjs = append(moduleObjs, moduleObj{name: name, objFile: cachedObj, cached: true})
					mu.Unlock()
					return
				}
			}

			// Cache miss — compile
			obj := compileModule(irText, "promise-mod-"+name)

			// Save to cache
			if contentCacheKey != "" {
				meta := &module.CacheMeta{
					Kind:     module.CacheKindLLVMModule,
					Name:     name,
					CacheKey: contentCacheKey,
				}
				for _, mi := range modInfos {
					if mi.EffectiveIRPrefix() == name {
						meta.InterfaceHash = mi.InterfaceHash
						if mi.SemaInfo != nil && mi.File != nil {
							meta.Symbols = sema.ExportedScope(mi.SemaInfo, mi.File).Names()
						}
						break
					}
				}
				_ = module.SaveBuildCache(cacheDir, contentCacheKey, meta, obj)
			}

			mu.Lock()
			moduleObjs = append(moduleObjs, moduleObj{name: name, objFile: obj})
			mu.Unlock()
		}(modName, modIR)
	}
	wg.Wait()

	// Clean up non-cached temp files after linking
	for _, mo := range moduleObjs {
		if !mo.cached {
			defer os.Remove(mo.objFile)
		}
	}

	// Compile per-instance .bc files (each generic type instantiation gets its own .bc).
	// Cache keys are derived from the type declaration hash, making them stable across
	// unrelated source changes.
	instIRs := result.InstanceIRs()
	// Coverage builds emit externally-linked counter globals (T0574), so a
	// coverage instance .bc must never be reused in a non-coverage build
	// (undefined-symbol link error) or vice versa (silent undercount). Mark
	// the instance cache mode so the two never share a build cache entry.
	instMode := buildModeStr(releaseMode)
	if result.CoverageEnabled() {
		instMode += "+cov"
	}
	instMetas := buildInstCacheMetas(result.SemaInfo(), compilerHash, target, instMode)

	type instObj struct {
		name    string
		objFile string
		cached  bool
	}
	var instObjs []instObj
	var instWg sync.WaitGroup
	var instMu sync.Mutex

	// Pre-cached instances: body generation was skipped (CompileWithCache), so they
	// won't appear in instIRs. Load their .bc directly from the build cache.
	for instName, instMeta := range instMetas {
		if _, hasBody := instIRs[instName]; hasBody {
			continue // body was generated — handled in the goroutine loop below
		}
		if cachedFile := module.LookupBuildCache(cacheDir, instMeta.CacheKey); cachedFile != "" {
			instObjs = append(instObjs, instObj{name: instName, objFile: cachedFile, cached: true})
		}
		// If the file has vanished (e.g., concurrent promise clean), the instance
		// won't be linked and the linker will report an undefined symbol — correct.
	}

	// Compile instances that had bodies generated (cache miss on pre-check, or
	// caching not applicable). Results are saved to cache for future builds.
	for instName, instIR := range instIRs {
		instWg.Add(1)
		go func(name, irText string) {
			defer instWg.Done()

			instMeta := instMetas[name] // nil if not cacheable

			if instMeta != nil {
				if cachedFile := module.LookupBuildCache(cacheDir, instMeta.CacheKey); cachedFile != "" {
					instMu.Lock()
					instObjs = append(instObjs, instObj{name: name, objFile: cachedFile, cached: true})
					instMu.Unlock()
					return
				}
			}

			// Cache miss — compile
			obj := compileModule(irText, "promise-inst-"+name)

			if instMeta != nil {
				_ = module.SaveBuildCache(cacheDir, instMeta.CacheKey, instMeta, obj)
			}

			instMu.Lock()
			instObjs = append(instObjs, instObj{name: name, objFile: obj})
			instMu.Unlock()
		}(instName, instIR)
	}
	instWg.Wait()

	optExtra := fmt.Sprintf("(%d modules, %d instances)", len(moduleIRs), len(instObjs))
	timePhase("opt", time.Since(tOpt), optExtra)

	for _, iobj := range instObjs {
		if !iobj.cached {
			defer os.Remove(iobj.objFile)
		}
	}

	// Collect all bitcode/object files for linking
	objFiles := []string{mainObj}
	for _, mo := range moduleObjs {
		objFiles = append(objFiles, mo.objFile)
	}
	for _, iobj := range instObjs {
		objFiles = append(objFiles, iobj.objFile)
	}

	// Link all files together (release: LTO linkers handle cross-module optimization;
	// debug: plain linking with --gc-sections for DCE)
	tLink := time.Now()
	if isDarwinTarget(target) {
		linkDarwinMulti(objFiles, target, outputFile, useLTO)
	} else if isWasmTarget(target) {
		linkWasmMulti(objFiles, target, outputFile, useLTO)
	} else if isWindowsTarget(target) {
		linkWindowsMulti(objFiles, target, outputFile)
	} else {
		linkLinuxMulti(objFiles, target, outputFile, useLTO)
	}
	timePhase("link", time.Since(tLink), "")
}

// buildInstCacheMetas builds a map from mono instance name (e.g., "Vector[int]")
// to a CacheMeta containing the stable cache key and debug metadata. Instances
// whose origin type has no hash (e.g., native/universe types) are omitted.
func buildInstCacheMetas(mainInfo *sema.Info, compilerHash, target, buildMode string) map[string]*module.CacheMeta {
	if mainInfo == nil {
		return nil
	}
	// B0244: Build a sorted list of module IR prefixes to include in instance
	// cache keys. This ensures cross-module vs module-internal compilations
	// of the same instance type get separate cache entries.
	var moduleContext []string
	if mainInfo.ModuleInfos != nil {
		for _, mi := range mainInfo.ModuleInfos {
			moduleContext = append(moduleContext, mi.EffectiveIRPrefix())
		}
		sort.Strings(moduleContext)
	}
	instances := codegen.CollectMonoInstances(mainInfo)
	result := make(map[string]*module.CacheMeta, len(instances))
	for _, inst := range instances {
		mName := codegen.MonoName(inst)
		var tn *types.TypeName
		switch o := inst.Origin().(type) {
		case *types.Named:
			tn = o.Obj()
		case *types.Enum:
			tn = o.Obj()
		default:
			continue
		}
		typeDeclHash, irPrefix := findDeclHashInInfo(tn, mainInfo)
		if typeDeclHash == "" {
			continue // not cacheable
		}
		key := module.InstanceCacheKey(irPrefix, mName, typeDeclHash, compilerHash, target, buildMode, moduleContext)
		result[mName] = &module.CacheMeta{
			Kind:         module.CacheKindInstance,
			Name:         mName,
			CacheKey:     key,
			TypeDeclHash: typeDeclHash,
			IRPrefix:     irPrefix,
		}
	}
	return result
}

// buildModeStr returns "release" or "debug" for use in cache keys.
func buildModeStr(releaseMode bool) string {
	if releaseMode {
		return "release"
	}
	return "debug"
}

// findDeclHashInInfo looks up the type decl hash for a TypeName.
// Searches the main info first, then all module infos.
// Returns (hash, irPrefix) where irPrefix is "" for types in the main file.
func findDeclHashInInfo(tn *types.TypeName, mainInfo *sema.Info) (string, string) {
	if h, ok := mainInfo.DeclHashes[tn]; ok {
		return h, ""
	}
	for _, mi := range mainInfo.ModuleInfos {
		if mi.SemaInfo == nil {
			continue
		}
		if h, ok := mi.SemaInfo.DeclHashes[tn]; ok {
			return h, mi.EffectiveIRPrefix()
		}
	}
	return "", ""
}

// lookupCachedInstances checks which generic type instances already have a
// cached .bc file and can be skipped during codegen. Returns a map of
// mono instance name → true for each cached instance. Returns nil when
// instance caching doesn't apply (no modules, clang pipeline, no cache dir).
func lookupCachedInstances(info *sema.Info, target, buildMode string) map[string]bool {
	// Instance caching only applies to the separate compilation (LTO) path.
	if len(info.ModuleInfos) == 0 || useClangPipeline(target) {
		return nil
	}
	cacheDir, _ := module.BuildCacheDir()
	if cacheDir == "" {
		return nil
	}
	metas := buildInstCacheMetas(info, module.CompilerHash(), target, buildMode)
	if len(metas) == 0 {
		return nil
	}
	cached := make(map[string]bool, len(metas))
	for name, meta := range metas {
		if module.LookupBuildCache(cacheDir, meta.CacheKey) != "" {
			cached[name] = true
		}
	}
	if len(cached) == 0 {
		return nil
	}
	return cached
}

// sanitizeTempPrefix replaces characters that are invalid in file names on
// Windows (and visually noisy elsewhere) with '_'. Mono instance names like
// "Vector[int?]" embed '?' which Windows rejects as a wildcard (T0425), and
// '*' / ':' / '<' / '>' / '|' / '"' / '/' / '\' are similarly reserved.
func sanitizeTempPrefix(prefix string) string {
	var b strings.Builder
	b.Grow(len(prefix))
	for _, r := range prefix {
		switch r {
		case '<', '>', ':', '"', '/', '\\', '|', '?', '*':
			b.WriteByte('_')
		default:
			if r < 0x20 {
				b.WriteByte('_')
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// compileLLToObj compiles LLVM IR text to an object file via opt + llc.
func compileLLToObj(irText, prefix, target, optPath, llcPath, optLevel string) string {
	prefix = sanitizeTempPrefix(prefix)
	llFile, err := os.CreateTemp("", prefix+"-*.ll")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating temp file: %v\n", err)
		os.Exit(1)
	}
	if _, err := fmt.Fprint(llFile, irText); err != nil {
		fmt.Fprintf(os.Stderr, "error writing IR: %v\n", err)
		os.Exit(1)
	}
	llFile.Close()
	defer os.Remove(llFile.Name())

	bcFile, err := os.CreateTemp("", prefix+"-*.bc")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating temp file: %v\n", err)
		os.Exit(1)
	}
	bcFile.Close()
	defer os.Remove(bcFile.Name())

	optCmd := runLLVMCmd(optPath, optLevel, llFile.Name(), "-o", bcFile.Name())
	optCmd.Stderr = os.Stderr
	if err := optCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error running opt on %s: %v\n", prefix, err)
		os.Exit(1)
	}

	// llc → .o
	objFile, err := os.CreateTemp("", prefix+"-*.o")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating temp file: %v\n", err)
		os.Exit(1)
	}
	objFile.Close()

	llcArgs := []string{"-mtriple=" + target, "-filetype=obj"}
	if isWasmTarget(target) {
		llcArgs = append(llcArgs, "-mattr=+bulk-memory,+mutable-globals,+sign-ext")
	} else if !isWindowsTarget(target) {
		llcArgs = append(llcArgs, "-function-sections", "-data-sections", "-relocation-model=pic")
	}
	llcArgs = append(llcArgs, bcFile.Name(), "-o", objFile.Name())

	llcCmd := runLLVMCmd(llcPath, llcArgs...)
	llcCmd.Stderr = os.Stderr
	if err := llcCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error running llc on %s: %v\n", prefix, err)
		os.Exit(1)
	}

	return objFile.Name()
}

// compileLLToBC compiles LLVM IR text to LLVM bitcode via opt.
// The bitcode is passed to LTO-capable linkers (ld.lld/wasm-ld/ld64.lld with --lto-O1).
func compileLLToBC(irText, prefix, optPath, optLevel string) string {
	prefix = sanitizeTempPrefix(prefix)
	llFile, err := os.CreateTemp("", prefix+"-*.ll")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating temp file: %v\n", err)
		os.Exit(1)
	}
	if _, err := fmt.Fprint(llFile, irText); err != nil {
		fmt.Fprintf(os.Stderr, "error writing IR: %v\n", err)
		os.Exit(1)
	}
	llFile.Close()
	defer os.Remove(llFile.Name())

	bcFile, err := os.CreateTemp("", prefix+"-*.bc")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating temp file: %v\n", err)
		os.Exit(1)
	}
	bcFile.Close()

	optCmd := runLLVMCmd(optPath, optLevel, llFile.Name(), "-o", bcFile.Name())
	optCmd.Stderr = os.Stderr
	if err := optCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error running opt on %s: %v\n", prefix, err)
		os.Exit(1)
	}

	return bcFile.Name()
}

// useClangPipeline returns true if the clang pipeline should be used instead of opt+llc+linker.
func useClangPipeline(target string) bool {
	if os.Getenv("PROMISE_USE_CLANG") == "1" {
		return true
	}
	// Linux, macOS, Windows, and WASM use the LLVM pipeline. Other platforms use clang.
	return !strings.Contains(target, "linux") && !strings.Contains(target, "macosx") &&
		!strings.Contains(target, "windows") && !strings.Contains(target, "wasm")
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

// --- Compiler stamp & extraction cache validity ---

// ensureCacheValidOnce runs the compiler stamp check at most once per process.
var ensureCacheValidOnce sync.Once

// ensureCacheValid checks whether the running compiler binary matches the one
// that last populated the extraction caches (LLVM tools, CRT, embedded catalog
// modules). If the binary has changed (new build, new version), all extraction
// caches are wiped so they will be re-populated from the current binary's
// embedded resources. The stamp is written after cleanup so subsequent runs
// of the same binary skip this entirely.
func ensureCacheValid() {
	ensureCacheValidOnce.Do(func() {
		changed, stamp := module.CompilerChanged()
		if !changed {
			return
		}
		// Compiler binary changed — clear all extraction caches.
		// Errors are non-fatal: worst case we re-extract on top of stale files.
		module.CleanLLVMCache()
		module.CleanCRTCache()
		module.CleanEmbeddedModuleCache()
		// Write the new stamp so the next invocation skips cleanup.
		if stamp != nil {
			module.WriteCompilerStamp(stamp)
		}
	})
}

// --- Embedded LLVM tool delivery (T0769) ---
//
// Heavy LLVM blobs are no longer extracted to a flat cache dir. Full-variant
// builds embed compressed blobs that `promise install` stages into the
// content-addressed store (stageEmbeddedLLVMBlobs); the runtime materializes a
// per-target view dir from the CAS (resolveLLVMView in llvm_cas.go). The codec
// is self-describing via the file extension: the dist-CAS publish path embeds
// the brotli <sha>.br directly (.br — T0807, smaller shipped artifact), while
// the dev/slim and Homebrew bundle paths embed .gz. The macOS Mach-O
// patch+re-sign lives in internal/blobstore. The embed FS and embeddedLLVMFiles
// list below are retained as the full-variant blob source.

// embeddedLLVMSymlinks maps lld-mode alias name → target ("lld") for the view dir.
var embeddedLLVMSymlinks = map[string]string{
	"ld.lld":   "lld",
	"ld64.lld": "lld",
	"lld-link": "lld",
	"wasm-ld":  "lld",
}

// llvmEmbeddedFiles returns all compressed blobs in the embedded LLVM FS — the
// brotli .br (publish path) or gzip .gz (dev/slim + Homebrew path); the runtime
// dispatches on the extension (decompressEmbeddedLLVM).
func llvmEmbeddedFiles() []string {
	if !hasEmbeddedLLVM || llvmEmbedPrefix == "" {
		return embeddedLLVMFiles
	}
	entries, err := embeddedLLVM.ReadDir(llvmEmbedPrefix)
	if err != nil {
		return embeddedLLVMFiles // fallback to static list
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".br") || strings.HasSuffix(e.Name(), ".gz") {
			files = append(files, e.Name())
		}
	}
	if len(files) == 0 {
		return embeddedLLVMFiles
	}
	return files
}

// --- LLVM tool pipeline ---

// findLLVMTool locates an LLVM tool (opt, llc, ld.lld, ld64.lld) by searching:
//  1. Sibling directory of the promise binary
//  2. Environment variable override (PROMISE_OPT, PROMISE_LLC, PROMISE_LLD, PROMISE_LD64LLD)
//  3. Content-addressed store — a pre-staged toolchain view (CAS hit only, no
//     network), preferred over a system LLVM so full builds stay deterministic
//  4. Homebrew LLVM (macOS)
//  5. Versioned names on PATH (e.g., opt-22, llc-22, ld.lld-22) from newest to minLLVMMajor
//  6. Unversioned names on PATH (e.g., opt, llc, ld.lld)
//  7. Content-addressed store — fetch the host toolchain on demand (last resort;
//     surfaces the §4.4 offline / broken-release error)
func findLLVMTool(name string) (string, error) {
	envMap := map[string]string{
		"opt":      "PROMISE_OPT",
		"llc":      "PROMISE_LLC",
		"ld.lld":   "PROMISE_LLD",
		"ld64.lld": "PROMISE_LD64LLD",
		"wasm-ld":  "PROMISE_WASM_LD",
		"lld-link": "PROMISE_LLD",
	}

	// On Windows, tools have .exe extension — try with suffix first, then bare name.
	// On other platforms, just search the bare name.
	searchNames := []string{name}
	if runtime.GOOS == "windows" && !strings.HasSuffix(name, ".exe") {
		searchNames = []string{name + ".exe", name}
	}

	// 1. Sibling of promise binary (also check llvm/ subdirectory for install layout)
	if execPath, err := os.Executable(); err == nil {
		dir := filepath.Dir(execPath)
		for _, n := range searchNames {
			for _, candidate := range []string{
				filepath.Join(dir, n),
				filepath.Join(dir, "llvm", n),
			} {
				if _, err := os.Stat(candidate); err == nil {
					return candidate, nil
				}
			}
		}
	}

	// 2. Env override
	if envName, ok := envMap[name]; ok {
		if p := os.Getenv(envName); p != "" {
			return p, nil
		}
	}

	// 3. Content-addressed store — pre-staged toolchain view, CAS hit only (no
	//    network). Prefer it over a system LLVM so full builds are deterministic.
	if viewDir, _ := resolveLLVMView(false); viewDir != "" {
		for _, n := range searchNames {
			p := filepath.Join(viewDir, n)
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

	// 7. Content-addressed store — fetch the host toolchain from the manifest
	//    sources (last resort). A fetch failure surfaces the §4.4 offline /
	//    broken-release error directly.
	if viewDir, ferr := resolveLLVMView(true); viewDir != "" {
		for _, n := range searchNames {
			p := filepath.Join(viewDir, n)
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		}
	} else if ferr != nil {
		return "", ferr
	}

	envName := envMap[name]
	hint := fmt.Sprintf("%s not found\n  searched: sibling of promise binary, $%s, dependency cache, Homebrew LLVM, PATH (%s-{%d..%d}, %s)\n  install LLVM %d+",
		name, envName, name, maxLLVMSearch, minLLVMMajor, name, minLLVMMajor)
	if runtime.GOOS == "darwin" {
		hint += " (brew install llvm lld)"
	} else {
		hint += " or set PROMISE_USE_CLANG=1 to use clang"
	}
	return "", fmt.Errorf("%s", hint)
}

// runLLVMCmd creates an exec.Cmd for an LLVM tool, setting the platform-appropriate
// library path env var so dynamically-linked tools can find libLLVM when running
// from the content-addressed view dir. Uses LD_LIBRARY_PATH on Linux,
// DYLD_LIBRARY_PATH on macOS (where it points at the patched+signed dylib copy).
func runLLVMCmd(toolPath string, args ...string) *exec.Cmd {
	cmd := exec.Command(toolPath, args...)
	detachFromConsole(cmd)
	// If the tool is in the CAS view dir, ensure the library path includes that
	// dir so it can find libLLVM alongside it.
	toolDir := filepath.Dir(toolPath)
	envKey := llvmLibEnvKeyRuntime()
	if llvmViewDir != "" && toolDir == llvmViewDir && envKey != "" {
		env := os.Environ()
		ldPath := os.Getenv(envKey)
		if ldPath != "" {
			ldPath = toolDir + ":" + ldPath
		} else {
			ldPath = toolDir
		}
		prefix := envKey + "="
		found := false
		for i, e := range env {
			if strings.HasPrefix(e, prefix) {
				env[i] = prefix + ldPath
				found = true
				break
			}
		}
		if !found {
			env = append(env, prefix+ldPath)
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

var (
	wasmLinkerVersionOnce   sync.Once
	wasmLinkerVersionCached int
)

// detectWasmLinkerMajor returns the major LLVM version of the wasm LTO linker
// (wasm-ld), cached after the first call. Returns 0 if it cannot be determined.
// Used to pick a wasm32 DataLayout compatible with the linker (T0764). Lazy so
// non-wasm builds never probe for wasm-ld.
func detectWasmLinkerMajor() int {
	wasmLinkerVersionOnce.Do(func() {
		p, err := findLLVMTool("wasm-ld")
		if err != nil {
			return
		}
		wasmLinkerVersionCached = llvmToolVersion(p)
	})
	return wasmLinkerVersionCached
}

func init() {
	// Let codegen pick a wasm32 DataLayout matching the wasm-ld version (T0764).
	codegen.WasmLinkerMajorVersion = detectWasmLinkerMajor
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
		if runtime.GOOS == "darwin" {
			fmt.Fprintf(os.Stderr, "  install LLVM %d+ (brew install llvm lld)\n", minLLVMMajor)
		} else {
			fmt.Fprintf(os.Stderr, "  install LLVM %d+ or set PROMISE_USE_CLANG=1 to use clang\n", minLLVMMajor)
		}
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

// bundledLibSystemTBD is a minimal hand-crafted TAPI TBD v4 stub describing
// /usr/lib/libSystem.B.dylib with only the symbols Promise programs reference.
// This allows linking without Xcode Command Line Tools installed. The TBD tells
// the linker these symbols exist; at runtime dyld resolves them from the real
// system dylib. Symbol list derived from PAL source audit + nm -u on compiled
// binaries. (T0178)
const bundledLibSystemTBD = `--- !tapi-tbd
tbd-version:     4
targets:         [ x86_64-macos, arm64-macos ]
install-name:    '/usr/lib/libSystem.B.dylib'
current-version: 1000
exports:
  - targets:     [ x86_64-macos, arm64-macos ]
    symbols:     [ _malloc, _free, _realloc, _malloc_size, _memset, _memcpy,
                   _memcmp, _strlen, _write, _read, _exit, __exit,
                   _pthread_create, _pthread_join, _pthread_attr_init,
                   _pthread_attr_destroy, _pthread_attr_setstacksize,
                   _pthread_attr_setguardsize, _pthread_mutex_init,
                   _pthread_mutex_lock, _pthread_mutex_unlock,
                   _pthread_mutex_destroy, _pthread_cond_init,
                   _pthread_cond_wait, _pthread_cond_signal,
                   _pthread_cond_broadcast, _pthread_cond_destroy,
                   _open, _close, _lseek, _unlink, _access, _mkdir, _rmdir,
                   _stat, _lstat,
                   _fcntl, _opendir, _closedir, _readdir,
                   _fork, _execvp, _dup2, _pipe, _waitpid, _kill, _getpid,
                   _socket, _bind, _listen, _accept, _connect, _send, _recv,
                   _shutdown, _getsockname, _getsockopt, _setsockopt,
                   _getaddrinfo, _freeaddrinfo, _inet_pton,
                   _signal, _sigaction, _sigaltstack,
                   _getenv, _setenv, _unsetenv, _environ,
                   _getcwd, _chdir, _getuid, _getpwuid, _gethostname,
                   _sysconf, _kqueue, _kevent, _nanosleep, _usleep,
                   _sin, _cos, _exp, _log, _pow, _sqrt, _fabs, _floor,
                   _ceil, _round, ___error, __tlv_bootstrap,
                   dyld_stub_binder ]
...
`

// ensureBundledSDK writes the bundled libSystem TBD stub to the Promise cache
// directory and returns an SDK info pointing to it. Used as fallback when
// xcrun is unavailable (no Xcode CLT installed). (T0178)
func ensureBundledSDK() (*macOSSDKInfo, error) {
	home, err := module.PromiseHome()
	if err != nil {
		return nil, fmt.Errorf("cannot determine Promise home: %w", err)
	}

	sdkDir := filepath.Join(home, "cache", "sdk", "macos")
	libDir := filepath.Join(sdkDir, "usr", "lib")
	tbdPath := filepath.Join(libDir, "libSystem.B.tbd")
	symlinkPath := filepath.Join(libDir, "libSystem.tbd")

	// Create directory structure.
	if err := os.MkdirAll(libDir, 0755); err != nil {
		return nil, fmt.Errorf("cannot create bundled SDK directory: %w", err)
	}

	// Write TBD file (skip if already exists with correct size).
	content := []byte(bundledLibSystemTBD)
	if info, err := os.Stat(tbdPath); err != nil || info.Size() != int64(len(content)) {
		if err := os.WriteFile(tbdPath, content, 0644); err != nil {
			return nil, fmt.Errorf("cannot write bundled libSystem.tbd: %w", err)
		}
	}

	// Create symlink libSystem.tbd → libSystem.B.tbd (ld64.lld resolves -lSystem
	// to libSystem.tbd in the sysroot).
	if _, err := os.Lstat(symlinkPath); err != nil {
		if err := os.Symlink("libSystem.B.tbd", symlinkPath); err != nil {
			return nil, fmt.Errorf("cannot create libSystem.tbd symlink: %w", err)
		}
	}

	return &macOSSDKInfo{sysroot: sdkDir}, nil
}

// findMacOSSDK discovers the macOS SDK sysroot. First tries xcrun (fast path
// when Xcode CLT is installed), then falls back to bundled libSystem TBD
// stubs so linking works without any external SDK. (T0178)
func findMacOSSDK() (*macOSSDKInfo, error) {
	// 1. Try xcrun (fast path — CLT installed).
	if sysroot, err := exec.Command("xcrun", "--show-sdk-path").Output(); err == nil {
		sysrootPath := strings.TrimSpace(string(sysroot))
		if sysrootPath != "" {
			if _, err := os.Stat(sysrootPath); err == nil {
				info := &macOSSDKInfo{sysroot: sysrootPath}
				if sdkVer, err := exec.Command("xcrun", "--show-sdk-version").Output(); err == nil {
					info.sdkVersion = strings.TrimSpace(string(sdkVer))
				}
				return info, nil
			}
		}
	}

	// 2. Fall back to bundled SDK stubs.
	return ensureBundledSDK()
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

// findDarwinLinker returns the path to ld64.lld for macOS linking.
// Apple's system ld bundles its own LLVM version which cannot read bitcode from
// newer LLVM versions (e.g., system ld has LLVM 17 but opt produces LLVM 22 bitcode),
// so we require ld64.lld (which is version-matched to the LLVM toolchain).
// Release builds embed lld; dev builds need it installed (e.g., brew install lld).
// Returns (path, isLLD, error).
func findDarwinLinker() (string, bool, error) {
	// 1. Try ld64.lld via standard LLVM tool discovery
	if path, err := findLLVMTool("ld64.lld"); err == nil {
		return path, true, nil
	}

	// 2. Environment override
	if p := os.Getenv("PROMISE_LD"); p != "" {
		return p, false, nil
	}

	hint := "ld64.lld not found (required for macOS linking)\n"
	hint += "  Apple's system ld cannot process LLVM 22+ bitcode\n"
	hint += "\n"
	hint += "  fix: install lld to get a version-matched LLVM linker\n"
	hint += "    brew install lld\n"
	hint += "  or: run bin/install-prereqs.sh"
	return "", false, fmt.Errorf("%s", hint)
}

// isDarwinTarget returns true if the target triple is macOS/Darwin.
func isDarwinTarget(target string) bool {
	return strings.Contains(target, "macosx")
}

// isWasmTarget returns true if the target triple is WebAssembly.
func isWasmTarget(target string) bool {
	return strings.Contains(target, "wasm")
}

// isWindowsTarget returns true if the target triple is Windows.
func isWindowsTarget(target string) bool {
	return strings.Contains(target, "windows")
}

// binaryExtension returns the file extension for compiled binaries on the target.
func binaryExtension(target string) string {
	if isWasmTarget(target) {
		return ".wasm"
	}
	if isWindowsTarget(target) {
		return ".exe"
	}
	return ""
}

// isTestExcluded checks if the current target matches any of the exclude identifiers.
func isTestExcluded(target string, excludes []string) bool {
	info := sema.ParseTargetInfo(target)
	for _, ex := range excludes {
		if sema.MatchTargetIdent(info, ex) {
			return true
		}
	}
	return false
}

// buildDarwinLinkArgs builds the linker argument list for macOS Mach-O linking.
// Works with ld64.lld and PROMISE_LD override linkers.
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
func buildLinuxLinkArgs(target, objFile, outputFile string, useLTO bool) []string {
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
	}
	if useLTO {
		args = append(args, "--lto-O1") // LTO: cross-module inlining and DCE
	} else {
		args = append(args, "--gc-sections") // DCE for non-LTO object files
	}
	args = append(args,
		"-m", emulationMode(target),
		"-pie",
		"-dynamic-linker", dynamicLinker(target),
		"-o", outputFile,
		// CRT startup (order matters)
		crt.scrt1,
		crt.crti,
		crt.crtbeginS,
	)

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
// 2. Installed location: <PROMISE_HOME>/lib/crt/{arch}/
// 3. Cache dir: <PROMISE_HOME>/cache/crt/{arch}/
// 4. Extract embedded CRT to cache (first build only)
func findMuslCRT(target string) (string, error) {
	// Ensure stale caches from a different compiler binary are cleared first.
	ensureCacheValid()

	arch := muslArchDir(target)

	// 1. Sibling of promise binary
	if execPath, err := os.Executable(); err == nil {
		dir := filepath.Join(filepath.Dir(execPath), "crt", arch)
		if muslCRTComplete(dir) {
			return dir, nil
		}
	}

	promiseHome, err := module.PromiseHome()
	if err != nil {
		return "", fmt.Errorf("cannot determine Promise home: %v", err)
	}

	// 2. Installed location (<PROMISE_HOME>/lib/crt/{arch}/)
	installDir := filepath.Join(promiseHome, "lib", "crt", arch)
	if muslCRTComplete(installDir) {
		return installDir, nil
	}

	// 3. Cache dir (<PROMISE_HOME>/cache/crt/{arch}/)
	cacheDir := filepath.Join(promiseHome, "cache", "crt", arch)

	if muslCRTValid(cacheDir) {
		return cacheDir, nil
	}

	// 4. Content-addressed store — resolve musl CRT blobs into a per-arch view
	//    dir, but only when the manifest carries musl entries (forward-looking;
	//    T0530 will host them). Returns "" otherwise so we fall through.
	if viewDir, verr := resolveMuslCRTView(arch); verr != nil {
		return "", verr
	} else if viewDir != "" {
		return viewDir, nil
	}

	// 5. Extract the embedded musl CRT to the cache dir (the working path until
	//    musl blobs are hosted; Linux binaries always embed the CRT).
	if !hasEmbeddedMuslCRT {
		return "", fmt.Errorf("musl CRT not available for %s\n  this binary was not built with embedded musl CRT and the manifest has no musl blobs\n  set PROMISE_USE_CLANG=1 to use clang with system glibc instead", arch)
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
func buildMuslLinkArgs(target, objFile, outputFile, crtDir string, useLTO bool) []string {
	args := []string{
		"-m", emulationMode(target),
		"-static",
		"--build-id",
		"--eh-frame-hdr",
	}
	if useLTO {
		args = append(args, "--lto-O1") // LTO: cross-module inlining and DCE
	} else {
		args = append(args, "--gc-sections") // DCE for non-LTO object files
	}
	args = append(args,
		"-o", outputFile,
		filepath.Join(crtDir, "crt1.o"),
		filepath.Join(crtDir, "crti.o"),
		objFile,
		filepath.Join(crtDir, "libc.a"),
		filepath.Join(crtDir, "crtn.o"),
	)
	return args
}

// compileAndLinkLLVM runs the opt + linker pipeline.
// Release: opt -O1 → .bc → linker with --lto-O1 (LTO handles cross-module DCE/inlining).
// Debug: opt -O1 → .bc → llc → .o → linker with --gc-sections (no LTO).
// Windows: always opt → .bc → llc → .o → lld-link (LTO not wired up for MSVC yet).
// Note: opt -O1 in both modes until B0314 (alloca domination) is fixed to enable -O0.
func compileAndLinkLLVM(llFile, target, outputFile string, releaseMode bool) {
	optPath, err := findLLVMTool("opt")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	checkLLVMToolVersion(optPath)

	useLTO := (releaseMode || isWasmTarget(target)) && !isWindowsTarget(target)
	optLevel := "-O1" // B0314: -O0 blocked by alloca domination issue

	// Step 1: opt (coroutine lowering + optimization → bitcode)
	bcFile, err := os.CreateTemp("", "promise-*.bc")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating temp file: %v\n", err)
		os.Exit(1)
	}
	bcFile.Close()
	defer os.Remove(bcFile.Name())

	tOpt := time.Now()
	optCmd := runLLVMCmd(optPath, optLevel, llFile, "-o", bcFile.Name())
	optCmd.Stderr = os.Stderr
	if err := optCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error running opt: %v\n", err)
		os.Exit(1)
	}
	timePhase("opt", time.Since(tOpt), "")

	if !useLTO {
		// Debug / Windows: llc → .o → linker (no LTO)
		llcPath, err := findLLVMTool("llc")
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		checkLLVMToolVersion(llcPath)

		objFile, err := os.CreateTemp("", "promise-*.o")
		if err != nil {
			fmt.Fprintf(os.Stderr, "error creating temp file: %v\n", err)
			os.Exit(1)
		}
		objFile.Close()
		defer os.Remove(objFile.Name())

		llcArgs := []string{"-mtriple=" + target, "-filetype=obj"}
		if isWasmTarget(target) {
			llcArgs = append(llcArgs, "-mattr=+bulk-memory,+mutable-globals,+sign-ext")
		} else if !isWindowsTarget(target) {
			llcArgs = append(llcArgs, "-function-sections", "-data-sections", "-relocation-model=pic")
		}
		llcArgs = append(llcArgs, bcFile.Name(), "-o", objFile.Name())

		llcCmd := runLLVMCmd(llcPath, llcArgs...)
		llcCmd.Stderr = os.Stderr
		if err := llcCmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "error running llc: %v\n", err)
			os.Exit(1)
		}

		tLink := time.Now()
		if isWindowsTarget(target) {
			linkWindows(objFile.Name(), target, outputFile)
		} else if isDarwinTarget(target) {
			linkDarwin(objFile.Name(), target, outputFile, false)
		} else if isWasmTarget(target) {
			linkWasm(objFile.Name(), target, outputFile, false)
		} else {
			linkLinux(objFile.Name(), target, outputFile, false)
		}
		timePhase("link", time.Since(tLink), "")
		return
	}

	// Release: Link with LTO — linker performs cross-module inlining and DCE on bitcode.
	tLink := time.Now()
	if isDarwinTarget(target) {
		linkDarwin(bcFile.Name(), target, outputFile, true)
	} else if isWasmTarget(target) {
		linkWasm(bcFile.Name(), target, outputFile, true)
	} else {
		linkLinux(bcFile.Name(), target, outputFile, true)
	}
	timePhase("link", time.Since(tLink), "")
}

// linkDarwin runs ld64.lld for macOS Mach-O linking.
// Accepts LLVM bitcode (.bc) or native object (.o) as input.
// Uses --lto-O1 for LTO when useLTO is true. The !isLLD path is only reachable via PROMISE_LD override.
func linkDarwin(bcOrObjFile, target, outputFile string, useLTO bool) {
	linkerPath, isLLD, err := findDarwinLinker()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if isLLD {
		checkLLVMToolVersion(linkerPath)
	}

	fileToLink := bcOrObjFile
	// PROMISE_LD override: non-LLD linker cannot process LLVM bitcode — run llc first.
	if !isLLD && strings.HasSuffix(bcOrObjFile, ".bc") {
		llcPath, lerr := findLLVMTool("llc")
		if lerr != nil {
			fmt.Fprintf(os.Stderr, "error: PROMISE_LD linker requires native object but llc not found: %v\n", lerr)
			os.Exit(1)
		}
		nativeObj, nerr := os.CreateTemp("", "promise-darwin-*.o")
		if nerr != nil {
			fmt.Fprintf(os.Stderr, "error creating temp file: %v\n", nerr)
			os.Exit(1)
		}
		nativeObj.Close()
		defer os.Remove(nativeObj.Name())
		llcArgs := []string{
			"-mtriple=" + target, "-filetype=obj",
			"-function-sections", "-relocation-model=pic",
			bcOrObjFile, "-o", nativeObj.Name(),
		}
		llcCmd := runLLVMCmd(llcPath, llcArgs...)
		llcCmd.Stderr = os.Stderr
		if err := llcCmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "error running llc for PROMISE_LD linker: %v\n", err)
			os.Exit(1)
		}
		fileToLink = nativeObj.Name()
	}

	linkArgs := buildDarwinLinkArgs(target, fileToLink, outputFile)
	if !useLTO {
		linkArgs = append([]string{"-dead_strip"}, linkArgs...) // DCE for non-LTO object files
	}
	var linkCmd *exec.Cmd
	if isLLD {
		if useLTO {
			linkArgs = append([]string{"--lto-O1"}, linkArgs...)
		}
		linkCmd = runLLVMCmd(linkerPath, linkArgs...)
	} else {
		linkCmd = exec.Command(linkerPath, linkArgs...)
		detachFromConsole(linkCmd)
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

// linkWasm runs wasm-ld for WebAssembly linking (single .o file).
func linkWasm(objFile, target, outputFile string, useLTO bool) {
	linkWasmMulti([]string{objFile}, target, outputFile, useLTO)
}

// ensureWasmAllocObj extracts the embedded WASM allocator object to cache.
// Returns the path to the .o file.
func ensureWasmAllocObj() (string, error) {
	// Ensure stale caches from a different compiler binary are cleared first.
	ensureCacheValid()

	promiseHome, err := module.PromiseHome()
	if err != nil {
		return "", fmt.Errorf("cannot determine Promise home: %v", err)
	}
	cacheDir := filepath.Join(promiseHome, "cache", "crt", "wasm32")
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

// ensureWasmMathObj extracts the embedded WASM math runtime object to cache.
// T0333: WASM linking uses --lto-O1 to avoid an LLVM 23 miscompile of `icmp
// samesign`, but at -O1 LLVM doesn't constant-fold sin/cos/etc. and instead
// emits libcalls. wasm_math.o provides those libcall symbols (sin, cos, tan,
// exp, log, pow, sqrt, fabs, floor, ceil, round + f32 variants) so unresolved
// imports don't appear in the final WASM module.
func ensureWasmMathObj() (string, error) {
	ensureCacheValid()

	promiseHome, err := module.PromiseHome()
	if err != nil {
		return "", fmt.Errorf("cannot determine Promise home: %v", err)
	}
	cacheDir := filepath.Join(promiseHome, "cache", "crt", "wasm32")
	objPath := filepath.Join(cacheDir, "wasm_math.o")

	if info, err := os.Stat(objPath); err == nil {
		if info.Size() == int64(len(embeddedWasmMathObj)) {
			return objPath, nil
		}
	}

	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", fmt.Errorf("cannot create WASM CRT cache: %v", err)
	}
	if err := os.WriteFile(objPath, embeddedWasmMathObj, 0644); err != nil {
		return "", fmt.Errorf("cannot write wasm_math.o to cache: %v", err)
	}
	return objPath, nil
}

// componentWrap wraps a core WASM binary into a Component Model binary using wasm-tools.
// If adaptPath is non-empty, it's passed as --adapt to wasm-tools component new.
// If adaptPath is empty and this is a WASI target, the embedded P1→P2 adapter is used
// (if available), otherwise wasm-tools is called without an adapter.
func componentWrap(coreWasm, outputFile, adaptPath string) {
	wasmToolsPath, err := findWasmTools()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n"+
			"Install wasm-tools: cargo install wasm-tools\n"+
			"Or set PROMISE_WASM_TOOLS to the binary path.\n", err)
		os.Exit(1)
	}

	args := []string{"component", "new", coreWasm, "-o", outputFile}

	// Use explicit adapter if provided, otherwise try embedded P1 adapter.
	if adaptPath != "" {
		args = append(args, "--adapt", adaptPath)
	} else if adapter, err := ensureWasiAdapter(); err == nil && adapter != "" {
		args = append(args, "--adapt", adapter)
	}

	cmd := exec.Command(wasmToolsPath, args...)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error wrapping component (wasm-tools): %v\n", err)
		os.Exit(1)
	}
}

// findWasmTools locates the wasm-tools binary.
// Checks PROMISE_WASM_TOOLS env var first, then PATH.
func findWasmTools() (string, error) {
	if env := os.Getenv("PROMISE_WASM_TOOLS"); env != "" {
		if _, err := os.Stat(env); err == nil {
			return env, nil
		}
		return "", fmt.Errorf("PROMISE_WASM_TOOLS=%q: file not found", env)
	}
	path, err := exec.LookPath("wasm-tools")
	if err != nil {
		return "", fmt.Errorf("wasm-tools not found in PATH")
	}
	return path, nil
}

// ensureWasiAdapter extracts the embedded WASI P1→P2 adapter to cache (if embedded).
// Returns the path to the adapter .wasm file, or empty string if not embedded.
func ensureWasiAdapter() (string, error) {
	if len(embeddedWasiAdapter) == 0 {
		return "", nil
	}

	ensureCacheValid()
	promiseHome, err := module.PromiseHome()
	if err != nil {
		return "", err
	}
	cacheDir := filepath.Join(promiseHome, "cache", "crt", "wasm32")
	adapterPath := filepath.Join(cacheDir, "wasi_snapshot_preview1.command.wasm")

	// Check if cached version matches embedded (by size)
	if info, err := os.Stat(adapterPath); err == nil {
		if info.Size() == int64(len(embeddedWasiAdapter)) {
			return adapterPath, nil
		}
	}

	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", err
	}
	if err := os.WriteFile(adapterPath, embeddedWasiAdapter, 0644); err != nil {
		return "", err
	}
	return adapterPath, nil
}

// linkWasmMulti links multiple .o files for WebAssembly.
func linkWasmMulti(objFiles []string, target, outputFile string, useLTO bool) {
	lldPath, err := findLLVMTool("wasm-ld")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	checkLLVMToolVersion(lldPath)

	linkArgs := buildWasmLinkArgs(objFiles, target, outputFile, useLTO)
	linkCmd := runLLVMCmd(lldPath, linkArgs...)
	linkCmd.Stderr = os.Stderr
	if err := linkCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error linking (wasm-ld): %v\n", err)
		os.Exit(1)
	}

	// Post-link: emit bootstrap .js loader for wasm32-web targets
	if strings.Contains(target, "web") {
		emitWebBootstrapJS(outputFile)
	}
}

// emitWebBootstrapJS writes a minimal JavaScript loader alongside the .wasm
// output for wasm32-web targets. The loader handles WebAssembly.instantiateStreaming
// and provides the basic runtime entry point. Module-specific JS glue (from
// promise bind webidl) should be imported separately.
func emitWebBootstrapJS(wasmFile string) {
	jsFile := strings.TrimSuffix(wasmFile, ".wasm") + ".js"
	wasmBase := filepath.Base(wasmFile)

	content := fmt.Sprintf(`// Auto-generated bootstrap loader for %s
// Import module-specific glue files separately if using WebIDL bindings.

export async function init(importObject = {}) {
  const response = await fetch("%s");
  const { instance } = await WebAssembly.instantiateStreaming(response, importObject);

  if (instance.exports._initialize) {
    instance.exports._initialize();
  }

  return instance.exports;
}
`, wasmBase, wasmBase)

	if err := os.WriteFile(jsFile, []byte(content), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write %s: %v\n", jsFile, err)
	}
}

// buildWasmLinkArgs builds the wasm-ld argument list for WASM linking.
// Links user code with the embedded free-list allocator (wasm_alloc.o) and the
// math runtime (wasm_math.o — sin/cos/exp/log/pow/etc. that LLVM lowers to
// libcalls but WASI doesn't supply).
// For wasm32-web: exports _initialize and memory instead of _start.
func buildWasmLinkArgs(objFiles []string, target, outputFile string, useLTO bool) []string {
	allocObj, err := ensureWasmAllocObj()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	mathObj, err := ensureWasmMathObj()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	isWeb := strings.Contains(target, "web")
	var args []string
	if useLTO {
		// T0333: --lto-O2 miscompiles loops where icmp gets the `samesign` flag
		// from opt's value-tracking pass (LLVM 23). The miscompilation causes the
		// loop exit comparison to skip exit, leading to OOB index reads. --lto-O1
		// avoids the buggy late-pass. We supply the math libcalls separately
		// (wasm_math.o) since --lto-O1 doesn't constant-fold sin/cos/etc.
		args = []string{"--lto-O1"}
	} else {
		args = []string{"--gc-sections"} // DCE for non-LTO object files
	}
	args = append(args,
		"--no-entry",
		"--allow-undefined", // WASI/JS imports resolved at runtime
		"-o", outputFile,
	)
	if isWeb {
		args = append(args, "--export=_initialize", "--export-memory")
	} else {
		args = append(args, "--export=_start")
	}
	// Canonical ABI: export cabi_realloc (required by Component Model for host-side
	// memory allocation) and __cabi_retarea (fixed buffer for multi-value returns).
	args = append(args, "--export=cabi_realloc", "--export=__cabi_retarea")
	args = append(args, objFiles...)
	args = append(args, allocObj, mathObj)
	return args
}

// linkLinux runs ld.lld for Linux ELF linking (glibc or musl).
func linkLinux(objFile, target, outputFile string, useLTO bool) {
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
		linkArgs = buildMuslLinkArgs(target, objFile, outputFile, crtDir, useLTO)
	} else {
		linkArgs = buildLinuxLinkArgs(target, objFile, outputFile, useLTO)
	}
	linkCmd := runLLVMCmd(lldPath, linkArgs...)
	linkCmd.Stderr = os.Stderr
	if err := linkCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error linking (ld.lld): %v\n", err)
		os.Exit(1)
	}
}

// linkDarwinMulti links multiple .o/.bc files on macOS.
func linkDarwinMulti(objFiles []string, target, outputFile string, useLTO bool) {
	linkerPath, isLLD, err := findDarwinLinker()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if isLLD {
		checkLLVMToolVersion(linkerPath)
	}

	sdk, err := findMacOSSDK()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	tri := parseDarwinTriple(target)
	sdkVersion := tri.minVersion
	if sdk.sdkVersion != "" {
		sdkVersion = sdk.sdkVersion
	}

	linkArgs := []string{
		"-arch", tri.arch,
		"-platform_version", "macos", tri.minVersion, sdkVersion,
		"-syslibroot", sdk.sysroot,
		"-o", outputFile,
	}
	linkArgs = append(linkArgs, objFiles...)
	linkArgs = append(linkArgs, "-lSystem")
	if !useLTO {
		linkArgs = append([]string{"-dead_strip"}, linkArgs...) // DCE for non-LTO object files
	}

	var linkCmd *exec.Cmd
	if isLLD {
		if useLTO {
			linkArgs = append([]string{"--lto-O1"}, linkArgs...)
		}
		linkCmd = runLLVMCmd(linkerPath, linkArgs...)
	} else {
		linkCmd = exec.Command(linkerPath, linkArgs...)
		detachFromConsole(linkCmd)
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

// linkLinuxMulti links multiple .o/.bc files on Linux (glibc or musl).
func linkLinuxMulti(objFiles []string, target, outputFile string, useLTO bool) {
	lldPath, err := findLLVMTool("ld.lld")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	checkLLVMToolVersion(lldPath)

	ltoOrGC := "--gc-sections"
	if useLTO {
		ltoOrGC = "--lto-O1"
	}

	var linkArgs []string
	if strings.Contains(target, "linux-musl") {
		crtDir, err := findMuslCRT(target)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		linkArgs = []string{
			"-m", emulationMode(target),
			"-static",
			"--build-id",
			"--eh-frame-hdr",
			ltoOrGC,
			"-o", outputFile,
			filepath.Join(crtDir, "crt1.o"),
			filepath.Join(crtDir, "crti.o"),
		}
		linkArgs = append(linkArgs, objFiles...)
		linkArgs = append(linkArgs,
			filepath.Join(crtDir, "libc.a"),
			filepath.Join(crtDir, "crtn.o"),
		)
	} else {
		crt, err := findCRT(target)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		linkArgs = []string{
			"-z", "relro",
			"--hash-style=gnu",
			"--build-id",
			"--eh-frame-hdr",
			ltoOrGC,
			"-m", emulationMode(target),
			"-pie",
			"-dynamic-linker", dynamicLinker(target),
			"-o", outputFile,
			crt.scrt1,
			crt.crti,
			crt.crtbeginS,
		}
		for _, dir := range crt.libDirs {
			linkArgs = append(linkArgs, "-L"+dir)
		}
		linkArgs = append(linkArgs, objFiles...)
		linkArgs = append(linkArgs,
			"-lpthread",
			"-lgcc", "--as-needed", "-lgcc_s", "--no-as-needed",
			"-lc",
			"-lgcc", "--as-needed", "-lgcc_s", "--no-as-needed",
		)
		linkArgs = append(linkArgs, crt.crtendS, crt.crtn)
	}

	linkCmd := runLLVMCmd(lldPath, linkArgs...)
	linkCmd.Stderr = os.Stderr
	if err := linkCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error linking (ld.lld): %v\n", err)
		os.Exit(1)
	}
}

// --- Windows linking via lld-link (MSVC-compatible COFF linker) ---

// winLinkFiles lists the self-generated Windows import libraries that make up
// the entire external link surface (T0772). Combined with the codegen-emitted
// crt0 (@__promise_start), TLS directory, __chkstk, and _fltused, these let a
// Promise .exe link with NO Visual Studio Build Tools / Windows SDK present and
// no Microsoft .lib redistribution. All resolve at runtime against DLLs that
// ship with every Windows install (kernel32/advapi32/ws2_32 are always present;
// ucrtbase.dll ships with Windows 10+).
var winLinkFiles = []string{"kernel32.lib", "advapi32.lib", "ws2_32.lib", "ucrtbase.lib"}

// winLinkArchDir returns the import-lib subdirectory for the given target
// triple. Only x86_64 is supported today (arm64 Windows is a follow-up).
func winLinkArchDir(target string) string {
	return "windows-amd64"
}

// winLinkComplete reports whether every import lib exists in dir.
func winLinkComplete(dir string) bool {
	for _, name := range winLinkFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return false
		}
	}
	return true
}

// winLinkValid checks that cached import libs match the embedded versions by
// size (cheap staleness check, mirroring muslCRTValid).
func winLinkValid(dir, arch string) bool {
	if !hasEmbeddedWinLink {
		return winLinkComplete(dir)
	}
	prefix := "resources/winlink/" + arch
	entries, err := embeddedWinLink.ReadDir(prefix)
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
	for _, name := range winLinkFiles {
		cached, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			return false
		}
		embSize, ok := embeddedSizes[name]
		if !ok || cached.Size() != embSize {
			return false
		}
	}
	return true
}

// findWindowsLinkSurface locates (extracting the embedded import libs to the
// cache on first use) the directory holding the self-generated Windows import
// libraries. Discovery order mirrors findMuslCRT: sibling of the binary,
// installed location, cache dir, then the embedded copy.
func findWindowsLinkSurface(target string) (string, error) {
	// Only x86_64 import libs are generated today (arm64 is a follow-up). Reject
	// an arm64 Windows triple here with a clear message rather than silently
	// handing lld-link the amd64 libs, which would fail with an opaque machine-
	// type mismatch against the arm64 objects (T0772).
	if strings.HasPrefix(target, "aarch64") || strings.HasPrefix(target, "arm64") {
		return "", fmt.Errorf("arm64 Windows is not yet supported by the self-generated link surface (T0772); only x86_64-pc-windows-msvc is available")
	}
	ensureCacheValid()
	arch := winLinkArchDir(target)

	// 1. Sibling of the promise binary: {exe_dir}/winlink/{arch}/
	if execPath, err := os.Executable(); err == nil {
		dir := filepath.Join(filepath.Dir(execPath), "winlink", arch)
		if winLinkComplete(dir) {
			return dir, nil
		}
	}

	promiseHome, err := module.PromiseHome()
	if err != nil {
		return "", fmt.Errorf("cannot determine Promise home: %v", err)
	}

	// 2. Installed location (<PROMISE_HOME>/lib/winlink/{arch}/)
	installDir := filepath.Join(promiseHome, "lib", "winlink", arch)
	if winLinkComplete(installDir) {
		return installDir, nil
	}

	// 3. Cache dir (<PROMISE_HOME>/cache/winlink/{arch}/)
	cacheDir := filepath.Join(promiseHome, "cache", "winlink", arch)
	if winLinkValid(cacheDir, arch) {
		return cacheDir, nil
	}

	// 4. Extract the embedded import libs to the cache dir.
	if !hasEmbeddedWinLink {
		return "", fmt.Errorf("Windows import libraries not available for %s\n  this binary was not built with the embedded Windows link surface", arch)
	}
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", fmt.Errorf("cannot create winlink cache dir %s: %v", cacheDir, err)
	}
	prefix := "resources/winlink/" + arch
	for _, name := range winLinkFiles {
		data, err := embeddedWinLink.ReadFile(prefix + "/" + name)
		if err != nil {
			return "", fmt.Errorf("cannot read embedded %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(cacheDir, name), data, 0644); err != nil {
			return "", fmt.Errorf("cannot write %s to cache: %v", name, err)
		}
	}
	return cacheDir, nil
}

// buildWindowsLinkArgs builds the lld-link argument list for COFF linking
// against the self-generated zero-dependency surface (T0772). Entry is the
// codegen-emitted @__promise_start (replacing the MSVC CRT's mainCRTStartup).
func buildWindowsLinkArgs(target string, objFiles []string, outputFile string) []string {
	libDir, err := findWindowsLinkSurface(target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	args := []string{
		"/nologo",
		"/entry:__promise_start",
		"/subsystem:console",
		"/out:" + outputFile,
		"/libpath:" + libDir,
	}
	args = append(args, winLinkFiles...)
	args = append(args, objFiles...)
	return args
}

// linkWindows runs lld-link for a single .obj file.
func linkWindows(objFile, target, outputFile string) {
	lldPath, err := findLLVMTool("lld-link")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	checkLLVMToolVersion(lldPath)

	linkArgs := buildWindowsLinkArgs(target, []string{objFile}, outputFile)
	linkCmd := runLLVMCmd(lldPath, linkArgs...)
	linkCmd.Stderr = os.Stderr
	if err := linkCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error linking (lld-link): %v\n", err)
		os.Exit(1)
	}
}

// linkWindowsMulti links multiple .obj files on Windows via lld-link.
func linkWindowsMulti(objFiles []string, target, outputFile string) {
	lldPath, err := findLLVMTool("lld-link")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	checkLLVMToolVersion(lldPath)

	linkArgs := buildWindowsLinkArgs(target, objFiles, outputFile)
	linkCmd := runLLVMCmd(lldPath, linkArgs...)
	linkCmd.Stderr = os.Stderr
	if err := linkCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error linking (lld-link): %v\n", err)
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
	tOptLink := time.Now()
	linkCmd := exec.Command(clang, linkArgs...)
	detachFromConsole(linkCmd)
	linkCmd.Stderr = os.Stderr
	if err := linkCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error linking (clang): %v\n", err)
		os.Exit(1)
	}
	timePhase("opt+link", time.Since(tOptLink), "(clang)")
}

// --- Frontend pipeline ---

// parseFile parses a .pr file and returns the AST.
// Source bytes are normalized to LF line endings before lexing so that
// triple-quoted and raw string literals have consistent content regardless
// of how git checked the file out (CRLF on Windows vs LF elsewhere).
func parseSourceFile(filename string) *ast.File {
	data, err := os.ReadFile(filename)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading %s: %v\n", filename, err)
		os.Exit(1)
	}
	source := strings.ReplaceAll(string(data), "\r\n", "\n")
	return parseSource(filename, source)
}

// parseSource parses already-read, CRLF-normalized source into an AST File.
func parseSource(filename, source string) *ast.File {
	input := antlr.NewInputStream(source)

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

// compileFrontend runs the full frontend pipeline for the host target.
func compileFrontend(filename string) (*ast.File, *sema.Info) {
	return compileFrontendForTarget(filename, "")
}

// compileProjectFrontend runs the full frontend pipeline on every non-test
// .pr file in a project directory, merging them into a single AST so symbols
// defined in one file are visible to the others. The project is anchored at
// projectDir/promise.toml; embed paths and module dependencies resolve
// relative to that root.
func compileProjectFrontend(projectDir string, files []string, triple string) (*ast.File, *sema.Info) {
	if len(files) == 0 {
		fmt.Fprintf(os.Stderr, "error: project %q contains no .pr files\n", projectDir)
		os.Exit(1)
	}

	tParse := time.Now()
	var merged *ast.File
	if home, homeErr := module.PromiseHome(); homeErr == nil {
		cacheDir := filepath.Join(home, "cache", "astcache")
		fileContents := make([][]byte, len(files))
		for i, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error reading %s: %v\n", f, err)
				os.Exit(1)
			}
			fileContents[i] = []byte(strings.ReplaceAll(string(data), "\r\n", "\n"))
		}
		contentHash := astcache.ContentHash(files, fileContents)
		key := astcache.Key(module.CompilerHash(), contentHash)
		if cached, _ := astcache.Load(cacheDir, key); cached != nil {
			merged = cached
		} else {
			parsed := make([]*ast.File, 0, len(files))
			for i, f := range files {
				parsed = append(parsed, parseSource(f, string(fileContents[i])))
			}
			merged = mergeModuleFiles(parsed)
			astcache.Save(cacheDir, key, merged)
		}
	} else {
		parsed := make([]*ast.File, 0, len(files))
		for _, f := range files {
			parsed = append(parsed, parseSourceFile(f))
		}
		merged = mergeModuleFiles(parsed)
	}
	timePhase("parse", time.Since(tParse), "")

	filterTriple := triple
	if filterTriple == "" {
		filterTriple = codegen.HostTargetTriple()
	}
	target := sema.ParseTargetInfo(filterTriple)

	merged = injectStdImport(merged)
	merged = injectGzipImportIfNeeded(merged)

	tSema := time.Now()

	tomlPath := filepath.Join(projectDir, "promise.toml")
	moduleScopes, modInfos, depOrder, modTiming := loadModuleScopes(tomlPath, merged, target)

	tUserSema := time.Now()
	info, errs := sema.CheckWithTarget(merged, moduleScopes, target)
	userSemaDur := time.Since(tUserSema)
	if modInfos != nil {
		info.ModuleInfos = modInfos
		info.ModuleOrder = depOrder
	}
	if len(errs) > 0 {
		timePhase("sema", time.Since(tSema), "")
		printFileErrors(projectDir, errs)
		os.Exit(1)
	}

	absProjectDir, _ := filepath.Abs(projectDir)
	embedErrs := sema.ResolveEmbeds(info, absProjectDir)
	timePhase("sema", time.Since(tSema), "")
	if timePhases {
		if modTiming != nil {
			parseExtra := fmt.Sprintf("(%d files)", modTiming.files)
			if modTiming.parseCached {
				parseExtra += " (cached)"
			}
			timeSubPhase("mod parse", modTiming.parseTime, parseExtra)
			mt := modTiming.timings
			timeSubPhase("mod sema", modTiming.semaTime,
				fmt.Sprintf("(declare: %dms, define: %dms, check: %dms, verify: %dms)",
					mt.Declare.Milliseconds(), mt.Define.Milliseconds(),
					mt.Check.Milliseconds(), mt.Verify.Milliseconds()))
		}
		ut := info.Timings
		timeSubPhase("user sema", userSemaDur,
			fmt.Sprintf("(declare: %dms, define: %dms, check: %dms, verify: %dms)",
				ut.Declare.Milliseconds(), ut.Define.Milliseconds(),
				ut.Check.Milliseconds(), ut.Verify.Milliseconds()))
	}
	if len(embedErrs) > 0 {
		printFileErrors(projectDir, embedErrs)
		os.Exit(1)
	}

	tOwner := time.Now()
	ownerErrs := ownership.Check(merged, info)
	timePhase("ownership", time.Since(tOwner), "")
	if len(ownerErrs) > 0 {
		printFileErrors(projectDir, ownerErrs)
		os.Exit(1)
	}

	return merged, info
}

// compileFrontendForTarget runs the full frontend pipeline: parse → merge std → sema → ownership.
// triple is the LLVM target triple used for `target(cond)` filtering (empty = host target).
func compileFrontendForTarget(filename, triple string) (*ast.File, *sema.Info) {
	tParse := time.Now()
	var file *ast.File
	// T0244: AST cache for user files.
	data, readErr := os.ReadFile(filename)
	if readErr != nil {
		fmt.Fprintf(os.Stderr, "error reading %s: %v\n", filename, readErr)
		os.Exit(1)
	}
	source := strings.ReplaceAll(string(data), "\r\n", "\n")
	if home, homeErr := module.PromiseHome(); homeErr == nil {
		cacheDir := filepath.Join(home, "cache", "astcache")
		contentHash := astcache.ContentHash([]string{filename}, [][]byte{[]byte(source)})
		key := astcache.Key(module.CompilerHash(), contentHash)
		if cached, _ := astcache.Load(cacheDir, key); cached != nil {
			file = cached
		} else {
			file = parseSource(filename, source)
			astcache.Save(cacheDir, key, file)
		}
	} else {
		file = parseSource(filename, source)
	}
	timePhase("parse", time.Since(tParse), "")

	// Resolve the effective target for `target(cond)` filtering.
	// Use the host triple when none is specified so platform-conditional functions
	// (e.g. `sep() string `target(windows)`) compile correctly without --target.
	filterTriple := triple
	if filterTriple == "" {
		filterTriple = codegen.HostTargetTriple()
	}
	target := sema.ParseTargetInfo(filterTriple)

	// Inject std as a glob import so all std symbols are available without explicit `use std;`
	file = injectStdImport(file)
	file = injectGzipImportIfNeeded(file)

	tSema := time.Now()

	// Load local modules from use declarations
	moduleScopes, modInfos, depOrder, modTiming := loadModuleScopes(filename, file, target)

	tUserSema := time.Now()
	info, errs := sema.CheckWithTarget(file, moduleScopes, target)
	userSemaDur := time.Since(tUserSema)
	if modInfos != nil {
		info.ModuleInfos = modInfos
		info.ModuleOrder = depOrder
	}
	if len(errs) > 0 {
		timePhase("sema", time.Since(tSema), "")
		printFileErrors(filename, errs)
		os.Exit(1)
	}

	// Resolve embed annotations: read files, validate contents
	absFilename, _ := filepath.Abs(filename)
	embedErrs := sema.ResolveEmbeds(info, filepath.Dir(absFilename))
	timePhase("sema", time.Since(tSema), "")
	if timePhases {
		if modTiming != nil {
			parseExtra := fmt.Sprintf("(%d files)", modTiming.files)
			if modTiming.parseCached {
				parseExtra += " (cached)"
			}
			timeSubPhase("mod parse", modTiming.parseTime, parseExtra)
			mt := modTiming.timings
			timeSubPhase("mod sema", modTiming.semaTime,
				fmt.Sprintf("(declare: %dms, define: %dms, check: %dms, verify: %dms)",
					mt.Declare.Milliseconds(), mt.Define.Milliseconds(),
					mt.Check.Milliseconds(), mt.Verify.Milliseconds()))
		}
		ut := info.Timings
		timeSubPhase("user sema", userSemaDur,
			fmt.Sprintf("(declare: %dms, define: %dms, check: %dms, verify: %dms)",
				ut.Declare.Milliseconds(), ut.Define.Milliseconds(),
				ut.Check.Milliseconds(), ut.Verify.Milliseconds()))
	}
	if len(embedErrs) > 0 {
		printFileErrors(filename, embedErrs)
		os.Exit(1)
	}

	tOwner := time.Now()
	ownerErrs := ownership.Check(file, info)
	timePhase("ownership", time.Since(tOwner), "")
	if len(ownerErrs) > 0 {
		printFileErrors(filename, ownerErrs)
		os.Exit(1)
	}

	return file, info
}

// cachedStdHash returns a stable hash of the embedded std library content.
// Derived from the embedded .sources.sha256 checksum file, computed once per process.
var (
	stdHashOnce sync.Once
	stdHashVal  string
)

func cachedStdHash() string {
	stdHashOnce.Do(func() {
		if len(embeddedSourcesChecksum) > 0 {
			h := fnv.New128a()
			h.Write(embeddedSourcesChecksum)
			stdHashVal = hex.EncodeToString(h.Sum(nil))
		}
	})
	return stdHashVal
}

// computeTestFileCacheKey computes a cache key for a non-module test file.
// Returns the key and true if cacheable, or ("", false) if not.
// The key covers: source content, compiler binary, std library, target triple,
// timeout configuration (B0132), and any local module dependencies (from sourced use declarations).
func computeTestFileCacheKey(filename, target string, cfg testTimeoutConfig) (string, bool) {
	content, err := os.ReadFile(filename)
	if err != nil {
		return "", false
	}

	fh := fnv.New128a()
	fh.Write(content)
	fileHash := hex.EncodeToString(fh.Sum(nil))
	compilerHash := module.CompilerHash()
	sHash := cachedStdHash()
	if sHash == "" {
		return "", false
	}

	h := fnv.New128a()
	fmt.Fprintf(h, "test-file:%s\n", fileHash)
	fmt.Fprintf(h, "compiler:%s\n", compilerHash)
	fmt.Fprintf(h, "std:%s\n", sHash)
	fmt.Fprintf(h, "target:%s\n", target)
	fmt.Fprintf(h, "mode:debug\n") // tests always use debug mode (T0205)
	fmt.Fprintf(h, "%s\n", cfg.cacheString())

	abs, _ := filepath.Abs(filename)
	dir := filepath.Dir(abs)

	// Hash embedded file contents — if any `embed("path") annotation references
	// an external file, its content must be part of the cache key so that changes
	// to embedded files invalidate the cache even when the .pr source is unchanged.
	// Handles single files, directory trees (...), and glob patterns (T0032).
	embedHashes, embedOK := module.HashEmbedFiles(content, dir)
	if !embedOK {
		return "", false
	}
	for _, eh := range embedHashes {
		fmt.Fprintf(h, "embed:%s\n", eh)
	}

	// Hash local module dependencies from sourced use declarations.
	useRe := regexp.MustCompile(`use\s+[\w_]+\s+"([^"]+)"`)
	matches := useRe.FindAllSubmatch(content, -1)
	if len(matches) > 0 {
		var modHashes []string
		for _, m := range matches {
			path := string(m[1])
			if strings.HasPrefix(path, "./") || strings.HasPrefix(path, "../") {
				modPath := filepath.Join(dir, path)
				modHash, err := module.HashModuleSources(modPath, false)
				if err != nil {
					return "", false
				}
				modHashes = append(modHashes, path+":"+modHash)
			} else {
				// Remote import — don't cache
				return "", false
			}
		}
		sort.Strings(modHashes)
		for _, mh := range modHashes {
			fmt.Fprintf(h, "mod:%s\n", mh)
		}
	}

	return hex.EncodeToString(h.Sum(nil)), true
}

// computeTestFileCacheInputs returns the list of inputs used in a test file's
// cache key computation for debug logging. Mirrors the logic in
// computeTestFileCacheKey without computing the key itself.
func computeTestFileCacheInputs(filename, target string, cfg testTimeoutConfig) []module.CacheKeyInput {
	content, err := os.ReadFile(filename)
	if err != nil {
		return nil
	}

	fh := fnv.New128a()
	fh.Write(content)
	fileHash := hex.EncodeToString(fh.Sum(nil))

	inputs := []module.CacheKeyInput{
		{Label: "file", Value: fileHash},
		{Label: "compiler", Value: module.CompilerHash()},
		{Label: "std", Value: cachedStdHash()},
		{Label: "target", Value: target},
		{Label: "timeout", Value: cfg.cacheString()},
	}

	abs, _ := filepath.Abs(filename)
	dir := filepath.Dir(abs)

	inputs = append(inputs, module.HashEmbedFilesForInputs(content, dir)...)

	useRe := regexp.MustCompile(`use\s+[\w_]+\s+"([^"]+)"`)
	for _, m := range useRe.FindAllSubmatch(content, -1) {
		path := string(m[1])
		if strings.HasPrefix(path, "./") || strings.HasPrefix(path, "../") {
			modPath := filepath.Join(dir, path)
			if modHash, err := module.HashModuleSources(modPath, false); err == nil {
				inputs = append(inputs, module.CacheKeyInput{
					Label: "dep " + path, Value: modHash,
				})
			}
		}
	}

	return inputs
}

// computeRunBinaryCacheKey computes a cache key for a `promise run` invocation.
// Returns the key and true if cacheable, or ("", false) if not (e.g., the source
// references remote modules or embedded files can't be read).
// The key covers: source content, compiler binary, std library, target triple,
// build mode (debug/release), embedded file contents, and local module deps.
func computeRunBinaryCacheKey(filename, target string, releaseMode bool) (string, bool) {
	content, err := os.ReadFile(filename)
	if err != nil {
		return "", false
	}

	fh := fnv.New128a()
	fh.Write(content)
	fileHash := hex.EncodeToString(fh.Sum(nil))
	compilerHash := module.CompilerHash()
	sHash := cachedStdHash()
	if sHash == "" {
		return "", false
	}

	h := fnv.New128a()
	fmt.Fprintf(h, "run-binary:%s\n", fileHash)
	fmt.Fprintf(h, "compiler:%s\n", compilerHash)
	fmt.Fprintf(h, "std:%s\n", sHash)
	fmt.Fprintf(h, "target:%s\n", target)
	fmt.Fprintf(h, "mode:%s\n", buildModeStr(releaseMode))

	abs, _ := filepath.Abs(filename)
	dir := filepath.Dir(abs)

	// Hash embedded file contents — if any `embed("path") annotation references
	// an external file, its content must be part of the cache key so that changes
	// to embedded files invalidate the cache even when the .pr source is unchanged.
	// Handles single files, directory trees (...), and glob patterns (T0032).
	embedHashes, embedOK := module.HashEmbedFiles(content, dir)
	if !embedOK {
		return "", false
	}
	for _, eh := range embedHashes {
		fmt.Fprintf(h, "embed:%s\n", eh)
	}

	// Hash local module dependencies from sourced use declarations.
	useRe := regexp.MustCompile(`use\s+[\w_]+\s+"([^"]+)"`)
	matches := useRe.FindAllSubmatch(content, -1)
	if len(matches) > 0 {
		var modHashes []string
		for _, m := range matches {
			path := string(m[1])
			if strings.HasPrefix(path, "./") || strings.HasPrefix(path, "../") {
				modPath := filepath.Join(dir, path)
				modHash, err := module.HashModuleSources(modPath, false)
				if err != nil {
					return "", false
				}
				modHashes = append(modHashes, path+":"+modHash)
			} else {
				// Remote import — don't cache
				return "", false
			}
		}
		sort.Strings(modHashes)
		for _, mh := range modHashes {
			fmt.Fprintf(h, "mod:%s\n", mh)
		}
	}

	return hex.EncodeToString(h.Sum(nil)), true
}

// computeRunBinaryCacheInputs returns the list of inputs used in a run binary's
// cache key computation for debug logging. Mirrors the logic in
// computeRunBinaryCacheKey without computing the key itself.
func computeRunBinaryCacheInputs(filename, target string, releaseMode bool) []module.CacheKeyInput {
	content, err := os.ReadFile(filename)
	if err != nil {
		return nil
	}

	fh := fnv.New128a()
	fh.Write(content)
	fileHash := hex.EncodeToString(fh.Sum(nil))

	inputs := []module.CacheKeyInput{
		{Label: "file", Value: fileHash},
		{Label: "compiler", Value: module.CompilerHash()},
		{Label: "std", Value: cachedStdHash()},
		{Label: "target", Value: target},
		{Label: "mode", Value: buildModeStr(releaseMode)},
	}

	abs, _ := filepath.Abs(filename)
	dir := filepath.Dir(abs)

	inputs = append(inputs, module.HashEmbedFilesForInputs(content, dir)...)

	useRe := regexp.MustCompile(`use\s+[\w_]+\s+"([^"]+)"`)
	for _, m := range useRe.FindAllSubmatch(content, -1) {
		path := string(m[1])
		if strings.HasPrefix(path, "./") || strings.HasPrefix(path, "../") {
			modPath := filepath.Join(dir, path)
			if modHash, err := module.HashModuleSources(modPath, false); err == nil {
				inputs = append(inputs, module.CacheKeyInput{
					Label: "dep " + path, Value: modHash,
				})
			}
		}
	}

	return inputs
}

// computeExecBinaryCacheKey computes a cache key for a `promise exec` invocation.
// Returns the key and true if cacheable, or ("", false) if not (e.g., the source
// references remote modules or embedded files can't be read).
//
// The source is the trimmed raw snippet *before* any autoInjectCatalogUses /
// main!()-wrap / injectStdImport transforms are applied — those are deterministic
// functions of (source, compiler), and compilerHash is already in the key, so the
// raw source is a sufficient and stable identity. exec is always debug mode, and
// embed/local-dep paths resolve against the current working directory (exec has no
// source file). The execution timeout is a pure runtime concern and is not part of
// the key. A distinct "exec-binary:" prefix prevents collisions with run-binary
// keys built from identical source bytes.
func computeExecBinaryCacheKey(source, target string) (string, bool) {
	content := []byte(source)

	fh := fnv.New128a()
	fh.Write(content)
	fileHash := hex.EncodeToString(fh.Sum(nil))
	compilerHash := module.CompilerHash()
	sHash := cachedStdHash()
	if sHash == "" {
		return "", false
	}

	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}

	h := fnv.New128a()
	fmt.Fprintf(h, "exec-binary:%s\n", fileHash)
	fmt.Fprintf(h, "compiler:%s\n", compilerHash)
	fmt.Fprintf(h, "std:%s\n", sHash)
	fmt.Fprintf(h, "target:%s\n", target)
	fmt.Fprintf(h, "mode:%s\n", buildModeStr(false)) // exec is always debug

	// Hash embedded file contents referenced from `embed(...) annotations so that
	// changes to embedded files invalidate the cache even when the source is unchanged.
	embedHashes, embedOK := module.HashEmbedFiles(content, dir)
	if !embedOK {
		return "", false
	}
	for _, eh := range embedHashes {
		fmt.Fprintf(h, "embed:%s\n", eh)
	}

	// Hash local module dependencies from sourced use declarations. Remote imports
	// (non-./, non-../) make the snippet non-cacheable, matching run semantics.
	useRe := regexp.MustCompile(`use\s+[\w_]+\s+"([^"]+)"`)
	matches := useRe.FindAllSubmatch(content, -1)
	if len(matches) > 0 {
		var modHashes []string
		for _, m := range matches {
			path := string(m[1])
			if strings.HasPrefix(path, "./") || strings.HasPrefix(path, "../") {
				modPath := filepath.Join(dir, path)
				modHash, err := module.HashModuleSources(modPath, false)
				if err != nil {
					return "", false
				}
				modHashes = append(modHashes, path+":"+modHash)
			} else {
				return "", false
			}
		}
		sort.Strings(modHashes)
		for _, mh := range modHashes {
			fmt.Fprintf(h, "mod:%s\n", mh)
		}
	}

	return hex.EncodeToString(h.Sum(nil)), true
}

// computeExecBinaryCacheInputs returns the list of inputs used in an exec binary's
// cache key computation for PROMISE_CACHE_DEBUG=verbose output. Mirrors the logic in
// computeExecBinaryCacheKey without computing the key itself.
func computeExecBinaryCacheInputs(source, target string) []module.CacheKeyInput {
	content := []byte(source)

	fh := fnv.New128a()
	fh.Write(content)
	fileHash := hex.EncodeToString(fh.Sum(nil))

	inputs := []module.CacheKeyInput{
		{Label: "file", Value: fileHash},
		{Label: "compiler", Value: module.CompilerHash()},
		{Label: "std", Value: cachedStdHash()},
		{Label: "target", Value: target},
		{Label: "mode", Value: buildModeStr(false)},
	}

	dir, err := os.Getwd()
	if err != nil {
		return inputs
	}

	inputs = append(inputs, module.HashEmbedFilesForInputs(content, dir)...)

	useRe := regexp.MustCompile(`use\s+[\w_]+\s+"([^"]+)"`)
	for _, m := range useRe.FindAllSubmatch(content, -1) {
		path := string(m[1])
		if strings.HasPrefix(path, "./") || strings.HasPrefix(path, "../") {
			modPath := filepath.Join(dir, path)
			if modHash, err := module.HashModuleSources(modPath, false); err == nil {
				inputs = append(inputs, module.CacheKeyInput{
					Label: "dep " + path, Value: modHash,
				})
			}
		}
	}

	return inputs
}

// computeProjectBinaryCacheKey computes a cache key for a `promise run` /
// `promise build` invocation against a project directory containing a
// promise.toml. The key covers all .pr files in the project tree (excluding
// tests), the compiler binary, std library, target triple, build mode,
// embedded files referenced from any project source, and local module deps.
func computeProjectBinaryCacheKey(projectDir, target string, releaseMode bool) (string, bool) {
	implHash, err := module.HashModuleSources(projectDir, false)
	if err != nil {
		return "", false
	}

	compilerHash := module.CompilerHash()
	sHash := cachedStdHash()
	if sHash == "" {
		return "", false
	}

	h := fnv.New128a()
	fmt.Fprintf(h, "project-binary:%s\n", implHash)
	fmt.Fprintf(h, "compiler:%s\n", compilerHash)
	fmt.Fprintf(h, "std:%s\n", sHash)
	fmt.Fprintf(h, "target:%s\n", target)
	fmt.Fprintf(h, "mode:%s\n", buildModeStr(releaseMode))

	if embedHash := module.HashModuleEmbeds(projectDir, false); embedHash != "" {
		fmt.Fprintf(h, "embed:%s\n", embedHash)
	}

	for _, dep := range scanModuleLocalDeps(projectDir) {
		fmt.Fprintf(h, "mod:%s\n", dep)
	}

	return hex.EncodeToString(h.Sum(nil)), true
}

// computeProjectBinaryCacheInputs mirrors computeProjectBinaryCacheKey for
// PROMISE_CACHE_DEBUG=verbose output.
func computeProjectBinaryCacheInputs(projectDir, target string, releaseMode bool) []module.CacheKeyInput {
	implHash, err := module.HashModuleSources(projectDir, false)
	if err != nil {
		return nil
	}
	inputs := []module.CacheKeyInput{
		{Label: "impl", Value: implHash},
		{Label: "compiler", Value: module.CompilerHash()},
		{Label: "std", Value: cachedStdHash()},
		{Label: "target", Value: target},
		{Label: "mode", Value: buildModeStr(releaseMode)},
	}
	if embedHash := module.HashModuleEmbeds(projectDir, false); embedHash != "" {
		inputs = append(inputs, module.CacheKeyInput{Label: "embed", Value: embedHash})
	}
	for _, dep := range scanModuleLocalDeps(projectDir) {
		inputs = append(inputs, module.CacheKeyInput{Label: "dep", Value: dep})
	}
	return inputs
}

// scanModuleLocalDeps scans all non-test .pr files in modDir for local module
// dependencies (use statements with relative paths). Returns a sorted list of
// "path:implhash" strings suitable for passing as depHashes to BuildCacheKey.
// Errors are silently ignored — the caller will get a cache miss and recompile.
func scanModuleLocalDeps(modDir string) []string {
	files, err := module.CollectModuleSources(modDir, false) // non-test sources only
	if err != nil {
		return nil
	}

	useRe := regexp.MustCompile(`use\s+[\w_]+\s+"([^"]+)"`)
	seen := map[string]bool{}
	var depHashes []string

	for _, path := range files {
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		matches := useRe.FindAllSubmatch(content, -1)
		for _, m := range matches {
			depPath := string(m[1])
			if !strings.HasPrefix(depPath, "./") && !strings.HasPrefix(depPath, "../") {
				continue // skip non-local (remote) imports
			}
			if seen[depPath] {
				continue
			}
			seen[depPath] = true
			absDepDir := filepath.Join(modDir, depPath)
			depHash, err := module.HashModuleSources(absDepDir, false)
			if err != nil {
				continue
			}
			depHashes = append(depHashes, depPath+":"+depHash)
		}
	}

	sort.Strings(depHashes)
	return depHashes
}

// isModuleTestFile checks whether filename is a *_test.pr file inside a module
// directory tree (a directory with promise.toml as ancestor). Returns the module
// root directory if found, empty string otherwise.
func isModuleTestFile(filename string) string {
	if !strings.HasSuffix(filepath.Base(filename), "_test.pr") {
		return ""
	}
	abs, err := filepath.Abs(filename)
	if err != nil {
		return ""
	}
	dir := filepath.Dir(abs)
	for {
		if _, err := os.Stat(filepath.Join(dir, "promise.toml")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// findEnclosingProjectDir walks up from a source file to the nearest ancestor
// directory containing a promise.toml, returning that directory, or "" if the
// file is not inside any project. Used so `promise build file.pr` / `run file.pr`
// builds the whole project instead of single-file-compiling (T0927).
func findEnclosingProjectDir(file string) string {
	abs, err := filepath.Abs(file)
	if err != nil {
		return ""
	}
	dir := filepath.Dir(abs)
	for {
		if _, err := os.Stat(filepath.Join(dir, "promise.toml")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// compileModuleTestFrontend compiles a module's test suite by merging all module
// source files (including *_test.pr files) into a single AST. This gives test
// functions access to all module-private declarations without needing `use <self>`.
// All test files in the module compile together (Go-style).
// triple is the LLVM target triple for `target(cond)` filtering (empty = no filtering).
func compileModuleTestFrontend(modDir, triple string) (*ast.File, *sema.Info) {
	// Read module config for name (used for self-import detection)
	modCfg, err := module.ParseConfig(filepath.Join(modDir, "promise.toml"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading module config: %v\n", err)
		os.Exit(1)
	}

	// Collect ALL .pr files (including tests) — walks subdirs, excludes nested modules
	allFiles, err := module.CollectModuleSources(modDir, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error collecting module sources: %v\n", err)
		os.Exit(1)
	}

	if len(allFiles) == 0 {
		fmt.Fprintf(os.Stderr, "error: module '%s' contains no .pr files\n", modCfg.Name)
		os.Exit(1)
	}

	// Parse all files
	var parsedFiles []*ast.File
	for _, f := range allFiles {
		parsedFiles = append(parsedFiles, parseSourceFile(f))
	}

	// Merge into single AST
	merged := mergeModuleFiles(parsedFiles)

	// Detect self-import: error if any file tries to `use <moduleName>`
	for _, u := range merged.Uses {
		if u.CatalogName == modCfg.Name && u.Path == "" {
			fmt.Fprintf(os.Stderr, "error: module test files should not `use %s;` — test code is compiled as part of the module\n", modCfg.Name)
			os.Exit(1)
		}
	}

	// Resolve the effective target for `target(cond)` filtering (host when unspecified).
	filterTriple := triple
	if filterTriple == "" {
		filterTriple = codegen.HostTargetTriple()
	}
	target := sema.ParseTargetInfo(filterTriple)

	// Inject std as a glob import
	merged = injectStdImport(merged)
	merged = injectGzipImportIfNeeded(merged)

	// Load module dependencies (the module's own `use` declarations)
	moduleScopes, modInfos, depOrder, _ := loadModuleScopes(filepath.Join(modDir, "promise.toml"), merged, target)

	info, errs := sema.CheckWithTarget(merged, moduleScopes, target)
	if modInfos != nil {
		info.ModuleInfos = modInfos
		info.ModuleOrder = depOrder
	}
	if len(errs) > 0 {
		printFileErrors(modDir, errs)
		os.Exit(1)
	}

	// Resolve embed annotations for module test files
	absModDir, _ := filepath.Abs(modDir)
	embedErrs := sema.ResolveEmbeds(info, absModDir)
	if len(embedErrs) > 0 {
		printFileErrors(modDir, embedErrs)
		os.Exit(1)
	}

	ownerErrs := ownership.Check(merged, info)
	if len(ownerErrs) > 0 {
		printFileErrors(modDir, ownerErrs)
		os.Exit(1)
	}

	return merged, info
}

// moduleLoader manages recursive module loading with cycle detection and caching.
// A single loader instance is shared across the entire dependency graph walk.
type moduleLoader struct {
	projectRoot string
	// projectCfg is the root project's parsed promise.toml.
	// Provides [require] pins and [replace] overrides for remote modules.
	projectCfg *module.Config
	// namedRequire maps import name → {url, commit} from [require.NAME] sections.
	// These allow non-catalog git dependencies to be imported by local name.
	namedRequire map[string]*module.RequireEntry
	// loaded caches fully loaded modules by absolute directory path.
	// This prevents re-loading the same module when multiple consumers import it.
	loaded map[string]*sema.ModuleInfo
	// globalIdentities maps global identity → absolute directory path.
	// Used to detect two different modules resolving to the same identity.
	globalIdentities map[string]string
	// visiting tracks modules currently being loaded (for cycle detection).
	// Maps absolute directory path → import path (for error messages).
	visiting map[string]string
	// visitStack records the import path order for cycle error messages.
	visitStack []string
	// allModInfos collects every module in the dependency graph for codegen.
	allModInfos map[string]*sema.ModuleInfo
	// depOrder records modules in topological order (dependencies before dependents).
	// This is the post-order of the DFS walk — leaf modules come first.
	depOrder []string
	// remoteResolved caches resolved remote URLs → absolute directory path.
	// Prevents re-fetching the same remote module.
	remoteResolved map[string]string
	// commitPins holds effective commit pins for remote modules.
	// Starts with [require] from root project, extended by transitive deps.
	commitPins map[string]string
	// warnings collects warning messages emitted during loading.
	warnings []string
	// catalog is the parsed embedded catalog manifest (nil if unavailable).
	catalog *module.Catalog
	// catalogLoaded caches catalog modules by name to prevent re-loading when a
	// file has both `use std as _` (auto-injected) and `use std;` (user-written).
	catalogLoaded map[string]*sema.ModuleInfo
	// target is the build target for `target(cond)` filtering in sema.
	target sema.TargetInfo
	// modParseTime accumulates parse time across all modules (T0215).
	modParseTime time.Duration
	// modSemaTime accumulates sema time across all modules (T0215).
	modSemaTime time.Duration
	// modFiles counts total .pr files parsed across all modules (T0215).
	modFiles int
	// modTimings accumulates per-pass sema timings across all modules (T0215).
	modTimings sema.SemaTimings
	// modParseCached is true if the std module AST was loaded from cache (T0214).
	modParseCached bool
}

// loadModuleScopes scans use declarations for local module paths, loads each
// module (parse + sema), and returns scopes for sema + ModuleInfos for codegen.
// Modules are loaded recursively: if module A imports module B, B is loaded first.
func loadModuleScopes(filename string, file *ast.File, target sema.TargetInfo) (map[string]*types.Scope, map[string]*sema.ModuleInfo, []string, *moduleTimings) {
	if len(file.Uses) == 0 {
		return nil, nil, nil, nil
	}

	// Find project root (directory containing promise.toml).
	// Fall back to the source file's directory for single-file mode.
	projectRoot := filepath.Dir(filename)
	if abs, err := filepath.Abs(projectRoot); err == nil {
		projectRoot = abs
	}
	var projectCfg *module.Config
	if cfg, err := module.FindConfig(projectRoot); err == nil && cfg != nil {
		projectRoot = cfg.Dir
		projectCfg = cfg
	}

	// Build initial commit pins from the root project's [require] section.
	commitPins := make(map[string]string)
	namedRequire := make(map[string]*module.RequireEntry)
	if projectCfg != nil {
		for url, pin := range projectCfg.Require {
			commitPins[module.NormalizeURL(url)] = pin
		}
		// Add commit pins from named require entries ([require.NAME] sections).
		for name, entry := range projectCfg.NamedRequire {
			commitPins[module.NormalizeURL(entry.URL)] = entry.Commit
			namedRequire[name] = entry
		}
	}

	// Parse embedded catalog
	var catalog *module.Catalog
	if len(embeddedCatalog) > 0 {
		cat, err := module.ParseCatalog(embeddedCatalog)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: invalid embedded catalog: %v\n", err)
			os.Exit(1)
		}
		catalog = cat
	}

	// Validate: named require entries must not conflict with catalog module names.
	for name := range namedRequire {
		if catalog != nil && catalog.Lookup(name) != nil {
			fmt.Fprintf(os.Stderr, "error: [require.%s] conflicts with catalog module '%s'\n", name, name)
			os.Exit(1)
		}
	}

	loader := &moduleLoader{
		projectRoot:      projectRoot,
		projectCfg:       projectCfg,
		namedRequire:     namedRequire,
		loaded:           make(map[string]*sema.ModuleInfo),
		globalIdentities: make(map[string]string),
		visiting:         make(map[string]string),
		allModInfos:      make(map[string]*sema.ModuleInfo),
		remoteResolved:   make(map[string]string),
		catalogLoaded:    make(map[string]*sema.ModuleInfo),
		commitPins:       commitPins,
		catalog:          catalog,
		target:           target,
	}

	scopes := make(map[string]*types.Scope)
	for _, u := range file.Uses {
		if u.Path == "" {
			// Check named require entries before catalog lookup.
			// This allows `use parser;` to resolve to a [require.parser] entry.
			var modInfo *sema.ModuleInfo
			var err error
			if entry, ok := loader.namedRequire[u.CatalogName]; ok {
				modInfo, err = loader.loadRemote(entry.URL, u.CatalogName)
			} else {
				modInfo, err = loader.loadCatalog(u.CatalogName)
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: error loading module '%s': %v\n", filename, u.CatalogName, err)
				os.Exit(1)
			}
			if modInfo != nil {
				if u.Alias != "_" {
					modInfo.Name = u.Alias
				}
				exportedScope := sema.ExportedScope(modInfo.SemaInfo, modInfo.File)
				modInfo.InterfaceHash = module.HashModuleInterface(exportedScope)
				scopes[u.CatalogName] = exportedScope
			}
			continue
		}

		var modInfo *sema.ModuleInfo
		var err error
		if module.IsLocalPath(u.Path) {
			modInfo, err = loader.load(u.Path)
		} else {
			modInfo, err = loader.loadRemote(u.Path, u.Alias)
		}
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
		exportedScope := sema.ExportedScope(modInfo.SemaInfo, modInfo.File)
		modInfo.InterfaceHash = module.HashModuleInterface(exportedScope)
		scopes[u.Path] = exportedScope
	}

	// Emit warnings collected during loading
	for _, w := range loader.warnings {
		fmt.Fprintln(os.Stderr, w)
	}

	if len(scopes) == 0 {
		return nil, nil, nil, nil
	}
	mt := &moduleTimings{
		parseTime:   loader.modParseTime,
		semaTime:    loader.modSemaTime,
		files:       loader.modFiles,
		timings:     loader.modTimings,
		parseCached: loader.modParseCached,
	}
	return scopes, loader.allModInfos, loader.depOrder, mt
}

// load recursively loads a local module and all its dependencies.
// Returns a cached result if the module was already loaded.
// Detects circular dependencies via the visiting set.
// modPath can be a relative path (joined with projectRoot) or an absolute path.
func (ml *moduleLoader) load(modPath string) (*sema.ModuleInfo, error) {
	// Resolve absolute directory for dedup and cycle detection
	var modDir string
	if filepath.IsAbs(modPath) {
		modDir = modPath
	} else {
		modDir = filepath.Join(ml.projectRoot, modPath)
	}
	absDir, err := filepath.Abs(modDir)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve path: %w", err)
	}

	// Check cache — already fully loaded
	if mi, ok := ml.loaded[absDir]; ok {
		return mi, nil
	}

	// Check for circular dependency
	if _, inProgress := ml.visiting[absDir]; inProgress {
		cycle := buildCyclePath(ml.visitStack, modPath)
		return nil, fmt.Errorf("circular dependency detected\n  %s", cycle)
	}

	// Mark as visiting (in progress)
	ml.visiting[absDir] = modPath
	ml.visitStack = append(ml.visitStack, modPath)
	defer func() {
		delete(ml.visiting, absDir)
		ml.visitStack = ml.visitStack[:len(ml.visitStack)-1]
	}()

	// Validate directory
	info, err := os.Stat(absDir)
	if err != nil {
		return nil, fmt.Errorf("module directory not found: %s", absDir)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("not a directory: %s", absDir)
	}

	// Parse promise.toml to get the module's canonical name
	tomlPath := filepath.Join(absDir, "promise.toml")
	modCfg, err := module.ParseConfig(tomlPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("module directory '%s' has no promise.toml", absDir)
		}
		return nil, fmt.Errorf("error reading promise.toml in '%s': %w", absDir, err)
	}

	// Compute globally unique identity for this module.
	// For local modules, this is the relative path from project root.
	// loadRemote() overrides this with the normalized URL for remote modules.
	globalID := module.GlobalIdentityForLocal(modPath)
	irPrefix := module.SanitizeIRPrefix(globalID)

	// Check for duplicate global identities — two different directories resolving to the same identity
	if existingDir, ok := ml.globalIdentities[globalID]; ok && existingDir != absDir {
		return nil, fmt.Errorf("duplicate module identity %q: resolved by both %s and %s", globalID, existingDir, absDir)
	}
	ml.globalIdentities[globalID] = absDir

	// Epoch mismatch warning
	if ml.projectCfg != nil && modCfg.Epoch != "" && ml.projectCfg.Epoch != "" && modCfg.Epoch != ml.projectCfg.Epoch {
		ml.warnings = append(ml.warnings, fmt.Sprintf("warning: module %q has epoch %s, but project uses epoch %s", modCfg.Name, modCfg.Epoch, ml.projectCfg.Epoch))
	}

	// Parse all .pr files in the module directory tree (skip *_test.pr files —
	// those are module tests, not part of the module's public implementation).
	// Walks subdirs recursively, excluding nested modules (subdirs with promise.toml).
	srcFiles, err := module.CollectModuleSources(absDir, false)
	if err != nil {
		return nil, err
	}

	// T0244: Binary AST cache for all modules — skip ANTLR4 parsing on cache hit.
	var merged *ast.File
	astCacheHit := false
	astCacheDir := ""
	astCacheKey := ""
	home, homeErr := module.PromiseHome()
	if homeErr == nil {
		astCacheDir = filepath.Join(home, "cache", "astcache")
		// Read all files for content hashing
		fileContents := make([][]byte, len(srcFiles))
		for i, f := range srcFiles {
			data, readErr := os.ReadFile(f)
			if readErr != nil {
				return nil, fmt.Errorf("error reading %s: %w", f, readErr)
			}
			fileContents[i] = []byte(strings.ReplaceAll(string(data), "\r\n", "\n"))
		}
		contentHash := astcache.ContentHash(srcFiles, fileContents)
		astCacheKey = astcache.Key(module.CompilerHash(), contentHash)
		tParse := time.Now()
		cached, _ := astcache.Load(astCacheDir, astCacheKey)
		if cached != nil {
			merged = cached
			astCacheHit = true
			ml.modParseCached = true
			ml.modParseTime += time.Since(tParse)
			ml.modFiles += len(srcFiles)
		} else {
			// Cache miss — parse from already-read contents
			modFileList := make([]*ast.File, 0, len(srcFiles))
			for i, f := range srcFiles {
				tParse2 := time.Now()
				modFileList = append(modFileList, parseSource(f, string(fileContents[i])))
				ml.modParseTime += time.Since(tParse2)
			}
			ml.modFiles += len(srcFiles)

			if len(modFileList) == 0 {
				return nil, fmt.Errorf("module '%s' contains no .pr files", modPath)
			}
			merged = mergeModuleFiles(modFileList)
			// T0244: Save AST cache on cache miss — before injectStdImport
			// so the cached AST doesn't include the injected `use std as _`.
			astcache.Save(astCacheDir, astCacheKey, merged)
		}
	}

	if !astCacheHit && merged == nil {
		// Fallback when PromiseHome is unavailable — parse without caching
		var modFileList []*ast.File
		for _, f := range srcFiles {
			tParse := time.Now()
			modFileList = append(modFileList, parseSourceFile(f))
			ml.modParseTime += time.Since(tParse)
		}
		ml.modFiles += len(srcFiles)

		if len(modFileList) == 0 {
			return nil, fmt.Errorf("module '%s' contains no .pr files", modPath)
		}
		merged = mergeModuleFiles(modFileList)
	}
	// Don't inject std into the std module itself
	if modCfg.Name != "std" {
		merged = injectStdImport(merged)
	}
	// Don't inject gzip into the gzip module itself (would cause self-import).
	if modCfg.Name != "gzip" {
		merged = injectGzipImportIfNeeded(merged)
	}

	// Recursively load this module's own dependencies
	depScopes, err := ml.loadDeps(merged, modPath)
	if err != nil {
		return nil, err
	}

	// Run sema on the module with its dependency scopes.
	var semaInfo *sema.Info
	var errs []error
	tModSema := time.Now()
	semaInfo, errs = sema.CheckWithTarget(merged, depScopes, ml.target)
	ml.modSemaTime += time.Since(tModSema)
	ml.modTimings.Declare += semaInfo.Timings.Declare
	ml.modTimings.Define += semaInfo.Timings.Define
	ml.modTimings.Check += semaInfo.Timings.Check
	ml.modTimings.Verify += semaInfo.Timings.Verify
	if len(errs) > 0 {
		return nil, fmt.Errorf("errors in module '%s': %v", modPath, errs[0])
	}

	// Resolve embed annotations for module implementation files (B0145)
	embedErrs := sema.ResolveEmbeds(semaInfo, absDir)
	if len(embedErrs) > 0 {
		return nil, fmt.Errorf("errors in module '%s': %v", modPath, embedErrs[0])
	}

	// Compute implementation hash from source files
	implHash, err := module.HashModuleSources(absDir, false)
	if err != nil {
		return nil, fmt.Errorf("cannot hash module sources: %w", err)
	}
	// Include embedded file contents in impl hash so data file changes
	// invalidate the module cache (T0032).
	if embedHash := module.HashModuleEmbeds(absDir, false); embedHash != "" {
		eh := fnv.New128a()
		fmt.Fprintf(eh, "%s\n%s", implHash, embedHash)
		implHash = hex.EncodeToString(eh.Sum(nil))
	}

	mi := &sema.ModuleInfo{
		Name:           modCfg.Name, // default to canonical name; consumer may override
		CanonicalName:  modCfg.Name, // display only (from promise.toml)
		GlobalIdentity: globalID,    // globally unique identity for dedup and cache
		IRPrefix:       irPrefix,    // sanitized prefix for IR symbols
		Path:           modPath,
		File:           merged,
		SemaInfo:       semaInfo,
		AbsDir:         absDir,
		ImplHash:       implHash,
	}

	// Cache the loaded module and register for codegen.
	// depOrder is post-order DFS: deps are added before dependents.
	ml.loaded[absDir] = mi
	ml.allModInfos[modPath] = mi
	ml.depOrder = append(ml.depOrder, modPath)
	return mi, nil
}

// loadRemote resolves a remote module URL to a local directory and loads it.
// Checks [replace] overrides first, then fetches via git using the commit pin.
func (ml *moduleLoader) loadRemote(remoteURL, alias string) (*sema.ModuleInfo, error) {
	normalized := module.NormalizeURL(remoteURL)

	// Check dedup cache — already resolved this URL
	if absDir, ok := ml.remoteResolved[normalized]; ok {
		if mi, ok := ml.loaded[absDir]; ok {
			return mi, nil
		}
	}

	// Check [replace] in root project config — redirect to local path
	if ml.projectCfg != nil {
		for replaceURL, localPath := range ml.projectCfg.Replace {
			if module.NormalizeURL(replaceURL) == normalized {
				// Resolve relative to project root
				if !filepath.IsAbs(localPath) {
					localPath = filepath.Join(ml.projectRoot, localPath)
				}
				mi, err := ml.load(localPath)
				if err != nil {
					return nil, fmt.Errorf("replace %s → %s: %w", remoteURL, localPath, err)
				}
				// Override identity: replaced remote modules use the remote URL identity.
				ml.overrideIdentity(mi, module.GlobalIdentityForRemote(normalized))

				ml.remoteResolved[normalized] = mi.AbsDir
				if err := ml.mergeTransitivePins(mi.AbsDir, remoteURL); err != nil {
					return nil, err
				}
				return mi, nil
			}
		}
	}

	// Look up commit pin
	pin, ok := ml.commitPins[normalized]
	if !ok {
		return nil, fmt.Errorf("remote module %q has no pin in promise.toml [require] section\n  hint: add '%s = \"<commit>\"' to [require], or run 'promise package pin \"%s\"'", remoteURL, remoteURL, remoteURL)
	}

	// Fetch/checkout via git
	absDir, err := module.ResolveRemoteModule(remoteURL, pin)
	if err != nil {
		return nil, err
	}

	ml.remoteResolved[normalized] = absDir

	// Delegate to load() which handles parsing, sema, cycle detection, etc.
	// Use the resolved absolute path directly.
	mi, err := ml.load(absDir)
	if err != nil {
		return nil, fmt.Errorf("remote module %s: %w", remoteURL, err)
	}

	// Override identity: remote modules use normalized URL as global identity,
	// not the local path that load() derived from the checkout directory.
	ml.overrideIdentity(mi, module.GlobalIdentityForRemote(normalized))

	if err := ml.mergeTransitivePins(absDir, remoteURL); err != nil {
		return nil, err
	}

	return mi, nil
}

// loadCatalog resolves a catalog import by looking up the name in the embedded
// catalog manifest. Embedded modules (no URL) are extracted from go:embed;
// external modules (with URL + commit) are fetched via git.
func (ml *moduleLoader) loadCatalog(catalogName string) (*sema.ModuleInfo, error) {

	// Return cached result to prevent re-loading the same catalog module when it
	// appears multiple times (e.g., auto-injected `use std as _` + user `use std;`).
	if mi, ok := ml.catalogLoaded[catalogName]; ok {
		return mi, nil
	}

	// Check [replace] — catalog name as key
	if ml.projectCfg != nil {
		if localPath, ok := ml.projectCfg.Replace[catalogName]; ok {
			if !filepath.IsAbs(localPath) {
				localPath = filepath.Join(ml.projectRoot, localPath)
			}
			ml.warnings = append(ml.warnings, fmt.Sprintf(
				"warning: catalog module '%s' replaced with local path %q\n  catalog compatibility guarantees do not apply to replaced modules",
				catalogName, localPath))
			mi, err := ml.load(localPath)
			if err != nil {
				return nil, fmt.Errorf("replace %s → %s: %w", catalogName, localPath, err)
			}
			// Override identity: use catalog identity, not local path identity
			ml.overrideIdentity(mi, module.GlobalIdentityForCatalog(catalogName))
			ml.catalogLoaded[catalogName] = mi
			return mi, nil
		}
	}

	// Look up in catalog
	if ml.catalog == nil {
		return nil, fmt.Errorf("unknown catalog module '%s' (no catalog available)", catalogName)
	}
	entry := ml.catalog.Lookup(catalogName)
	if entry == nil {
		return nil, fmt.Errorf("unknown catalog module '%s' — not in catalog for epoch %s", catalogName, ml.catalog.Epoch)
	}

	var absDir string

	if entry.IsEmbedded() {
		// Embedded module: extract from go:embed to a temp directory
		dir, err := extractEmbeddedModule(catalogName)
		if err != nil {
			return nil, fmt.Errorf("cannot load embedded catalog module '%s': %w", catalogName, err)
		}
		absDir = dir
	} else {
		// External module: fetch/checkout via git
		dir, err := module.ResolveRemoteModule(entry.URL, entry.Commit)
		if err != nil {
			return nil, fmt.Errorf("cannot fetch catalog module '%s': %w", catalogName, err)
		}
		absDir = dir
	}

	// Catalog modules must not have remote dependencies — they can only
	// depend on other catalog modules (resolved via their own use declarations).
	cfg, cfgErr := module.ParseConfig(filepath.Join(absDir, "promise.toml"))
	if cfgErr == nil && len(cfg.Require) > 0 {
		return nil, fmt.Errorf("catalog module '%s' has [require] entries in promise.toml — catalog modules can only depend on other catalog modules", catalogName)
	}

	// Delegate to load() for parsing, sema, cycle detection, caching
	mi, err := ml.load(absDir)
	if err != nil {
		return nil, fmt.Errorf("catalog module '%s': %w", catalogName, err)
	}

	// Override identity: catalog modules use their name as global identity
	ml.overrideIdentity(mi, module.GlobalIdentityForCatalog(catalogName))

	ml.catalogLoaded[catalogName] = mi
	return mi, nil
}

// extractEmbeddedModule extracts an embedded catalog module from go:embed to a
// persistent cache directory (~/.promise/cache/embedded_modules/<name>/).
// The compiler stamp mechanism (ensureCacheValid) clears these when the binary
// changes, so within a binary version the cache is always valid.
func extractEmbeddedModule(name string) (string, error) {
	// Ensure stale caches from a different compiler binary are cleared first.
	ensureCacheValid()

	prefix := "resources/modules/" + name
	entries, err := embeddedModules.ReadDir(prefix)
	if err != nil {
		return "", fmt.Errorf("no embedded source for module '%s'", name)
	}

	cacheDir, err := module.EmbeddedModuleCacheDir(name)
	if err != nil {
		return "", err
	}

	// Fast path: if the directory already exists with files, reuse it.
	// The compiler stamp guarantees these are from the current binary.
	if info, err := os.ReadDir(cacheDir); err == nil && len(info) > 0 {
		return cacheDir, nil
	}

	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", err
	}

	for _, e := range entries {
		if e.IsDir() {
			continue // embedded catalog modules are flat (no subdirectories)
		}
		data, err := embeddedModules.ReadFile(prefix + "/" + e.Name())
		if err != nil {
			return "", fmt.Errorf("reading embedded %s/%s: %w", name, e.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(cacheDir, e.Name()), data, 0644); err != nil {
			return "", err
		}
	}

	return cacheDir, nil
}

// overrideIdentity replaces a module's identity (GlobalIdentity + IRPrefix) and
// updates the globalIdentities dedup map atomically.
func (ml *moduleLoader) overrideIdentity(mi *sema.ModuleInfo, globalID string) {
	oldID := mi.GlobalIdentity
	mi.GlobalIdentity = globalID
	mi.IRPrefix = module.SanitizeIRPrefix(globalID)
	delete(ml.globalIdentities, oldID)
	ml.globalIdentities[mi.GlobalIdentity] = mi.AbsDir
}

// mergeTransitivePins reads a module's promise.toml and merges its [require] entries
// into the loader's effective commit pins. Top-level pins take priority; conflicting
// transitive pins produce an error.
func (ml *moduleLoader) mergeTransitivePins(absDir, sourceURL string) error {
	tomlPath := filepath.Join(absDir, "promise.toml")
	modCfg, cfgErr := module.ParseConfig(tomlPath)
	if cfgErr != nil {
		return nil // no config or parse error — nothing to merge
	}
	for depURL, depPin := range modCfg.Require {
		depNorm := module.NormalizeURL(depURL)
		if ml.isTopLevelPin(depNorm) {
			continue
		}
		if existing, exists := ml.commitPins[depNorm]; exists && existing != depPin {
			return fmt.Errorf("conflicting pins for %q: module %q pins %s but another module pins %s\n  hint: add an explicit pin in your project's [require] to resolve the conflict", depURL, sourceURL, depPin, existing)
		}
		ml.commitPins[depNorm] = depPin
	}
	return nil
}

// isTopLevelPin returns true if the normalized URL is pinned by the root project's [require].
func (ml *moduleLoader) isTopLevelPin(normalizedURL string) bool {
	if ml.projectCfg == nil {
		return false
	}
	for topURL := range ml.projectCfg.Require {
		if module.NormalizeURL(topURL) == normalizedURL {
			return true
		}
	}
	return false
}

// loadDeps scans a module's use declarations and recursively loads its dependencies.
// Returns module scopes for sema.CheckWithModules.
func (ml *moduleLoader) loadDeps(file *ast.File, parentPath string) (map[string]*types.Scope, error) {
	if len(file.Uses) == 0 {
		return nil, nil
	}

	scopes := make(map[string]*types.Scope)
	for _, u := range file.Uses {
		if u.Path == "" {
			// Check named require entries before catalog lookup.
			var depInfo *sema.ModuleInfo
			var err error
			if entry, ok := ml.namedRequire[u.CatalogName]; ok {
				depInfo, err = ml.loadRemote(entry.URL, u.CatalogName)
			} else {
				depInfo, err = ml.loadCatalog(u.CatalogName)
			}
			if err != nil {
				return nil, fmt.Errorf("in module '%s': %w", parentPath, err)
			}
			if depInfo != nil {
				exportedScope := sema.ExportedScope(depInfo.SemaInfo, depInfo.File)
				depInfo.InterfaceHash = module.HashModuleInterface(exportedScope)
				scopes[u.CatalogName] = exportedScope
			}
			continue
		}

		var depInfo *sema.ModuleInfo
		var err error
		if module.IsLocalPath(u.Path) {
			depInfo, err = ml.load(u.Path)
		} else {
			depInfo, err = ml.loadRemote(u.Path, u.Alias)
		}
		if err != nil {
			return nil, fmt.Errorf("in module '%s': %w", parentPath, err)
		}
		exportedScope := sema.ExportedScope(depInfo.SemaInfo, depInfo.File)
		depInfo.InterfaceHash = module.HashModuleInterface(exportedScope)
		scopes[u.Path] = exportedScope
	}

	if len(scopes) == 0 {
		return nil, nil
	}
	return scopes, nil
}

// buildCyclePath formats a circular dependency error showing the cycle.
// e.g., "a → b → c → a"
func buildCyclePath(stack []string, target string) string {
	// Find where the cycle starts in the stack
	start := -1
	for i, p := range stack {
		if p == target {
			start = i
			break
		}
	}
	if start < 0 {
		// Target not in stack — shouldn't happen, but handle gracefully
		return strings.Join(stack, " → ") + " → " + target
	}
	// Copy the cycle slice to avoid corrupting the caller's stack via append.
	cycle := make([]string, len(stack[start:])+1)
	copy(cycle, stack[start:])
	cycle[len(cycle)-1] = target
	return strings.Join(cycle, " → ")
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

// injectStdImport returns a shallow copy of the file with a `use std as _;` UseDecl prepended.
// This makes all std library symbols available without requiring an explicit `use std;`.
func injectStdImport(file *ast.File) *ast.File {
	stdUse := &ast.UseDecl{
		Alias:       "_",
		CatalogName: "std",
	}
	result := *file // shallow copy
	result.Uses = append([]*ast.UseDecl{stdUse}, file.Uses...)
	return &result
}

// fileUsesEmbedCompress reports whether any FuncDecl in the file has an
// `embed("...", compress: true) annotation. Used to decide whether to inject
// the gzip module for runtime decompression.
func fileUsesEmbedCompress(file *ast.File) bool {
	for _, d := range file.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		for _, ann := range fd.Annotations {
			if ann.Name != "embed" {
				continue
			}
			for _, p := range ann.Params {
				if p.Name != "compress" {
					continue
				}
				if bl, ok := p.Value.(*ast.BoolLit); ok && bl.Value {
					return true
				}
			}
		}
	}
	return false
}

// injectGzipImportIfNeeded prepends `use gzip as _gzip;` to the file's UseDecls
// when any embed getter requests `compress: true`. The aliased glob name
// `_gzip` is reserved for the codegen-emitted decompression call site so the
// user's symbol namespace is not polluted.
func injectGzipImportIfNeeded(file *ast.File) *ast.File {
	if !fileUsesEmbedCompress(file) {
		return file
	}
	gzipUse := &ast.UseDecl{
		Alias:       "_gzip",
		CatalogName: "gzip",
	}
	result := *file // shallow copy
	result.Uses = append([]*ast.UseDecl{gzipUse}, file.Uses...)
	return &result
}

type errorListener struct {
	antlr.DefaultErrorListener
	filename   string
	source     string // non-empty for inline mode: show source context
	wrapOffset int    // lines injected before user code (main!() wrapper + extracted uses)
	silent     bool   // if true, count errors but suppress output
	errors     int
}

func (l *errorListener) SyntaxError(
	_ antlr.Recognizer,
	_ interface{},
	line, column int,
	msg string,
	_ antlr.RecognitionException,
) {
	if !l.silent {
		if l.errors < 13 {
			if l.source != "" {
				lines := strings.Split(l.source, "\n")
				displayLine := line - l.wrapOffset
				fmt.Fprintf(os.Stderr, "%d:%d: %s\n", displayLine, column, msg)
				printErrorContext(lines, line-1, column)
			} else {
				fmt.Fprintf(os.Stderr, "%s:%d:%d: %s\n", l.filename, line, column, msg)
				lines := readFileLines(l.filename)
				if lines != nil {
					printErrorContext(lines, line-1, column)
				}
			}
		} else if l.errors == 13 {
			if l.filename != "" {
				fmt.Fprintf(os.Stderr, "%s: too many errors, suppressing remaining\n", l.filename)
			} else {
				fmt.Fprintln(os.Stderr, "too many errors, suppressing remaining")
			}
		}
	}
	l.errors++
}

// --- Inline execution ---

// extractUseDecls splits leading use declarations from the remaining source.
// Returns (useDecls, remainder) where useDecls contains all leading "use ...;"
// lines joined by newlines, and remainder is the rest of the source.
func extractUseDecls(source string) (string, string) {
	var uses []string
	rest := source
	for {
		trimmed := strings.TrimLeft(rest, " \t\n\r")
		if !strings.HasPrefix(trimmed, "use ") {
			break
		}
		idx := strings.Index(trimmed, ";")
		if idx < 0 {
			break
		}
		decl := trimmed[:idx+1]
		// Skip use-var bindings (use x := expr;) — those are statements, not imports.
		if strings.Contains(decl, ":=") {
			break
		}
		uses = append(uses, strings.TrimSpace(decl))
		// Advance rest past what we consumed (leading whitespace + use decl).
		consumed := len(rest) - len(trimmed) + idx + 1
		rest = rest[consumed:]
	}
	if len(uses) == 0 {
		return "", source
	}
	return strings.Join(uses, "\n"), strings.TrimSpace(rest)
}

// findLeadingUseBindings walks tokens from the start, collecting every IDENT
// that appears inside a leading `use ... ;` declaration. Stops at the first
// non-USE token. Matches extractUseDecls' leading-only semantics so that names
// bound via `use math;`, `use math as m;`, `use x "./x";`, etc. are all captured.
func findLeadingUseBindings(tokens []antlr.Token) map[string]bool {
	bindings := make(map[string]bool)
	i := 0
	for i < len(tokens) && tokens[i].GetTokenType() == parser.PromiseLexerUSE {
		i++ // past USE
		for i < len(tokens) && tokens[i].GetTokenType() != parser.PromiseLexerSEMI {
			if tokens[i].GetTokenType() == parser.PromiseLexerIDENT {
				bindings[tokens[i].GetText()] = true
			}
			i++
		}
		if i < len(tokens) { // past SEMI
			i++
		}
	}
	return bindings
}

// findCatalogRefs scans tokens for `IDENT .` patterns (not preceded by `.`)
// where IDENT matches a catalog module name. Returns the set of referenced
// catalog module names. Excludes std (auto-imported elsewhere) and non-embedded
// entries (avoids silent network fetches during exec).
func findCatalogRefs(tokens []antlr.Token, catalog *module.Catalog) map[string]bool {
	if catalog == nil {
		return nil
	}
	valid := make(map[string]bool)
	for name, entry := range catalog.Modules {
		if name == "std" || !entry.IsEmbedded() {
			continue
		}
		valid[name] = true
	}
	if len(valid) == 0 {
		return nil
	}
	refs := make(map[string]bool)
	for i := 0; i+1 < len(tokens); i++ {
		if tokens[i].GetTokenType() != parser.PromiseLexerIDENT {
			continue
		}
		if tokens[i+1].GetTokenType() != parser.PromiseLexerDOT {
			continue
		}
		if i > 0 && tokens[i-1].GetTokenType() == parser.PromiseLexerDOT {
			continue
		}
		if valid[tokens[i].GetText()] {
			refs[tokens[i].GetText()] = true
		}
	}
	return refs
}

// autoInjectCatalogUses prepends `use <name>;` declarations for each catalog
// module referenced via `name.X` member access but not already explicitly
// imported. No-op when the catalog is unavailable or nothing is missing.
// Token-level detection runs on the raw source before parsing, so it works
// regardless of whether the snippet parses as a full file or needs `main!()`
// wrapping. Known limitation: refs inside string interpolations (`"{io.x}"`)
// are not detected — the user must add an explicit `use` in that case.
func autoInjectCatalogUses(source string) string {
	if len(embeddedCatalog) == 0 {
		return source
	}
	cat, err := module.ParseCatalog(embeddedCatalog)
	if err != nil || cat == nil {
		return source
	}
	input := antlr.NewInputStream(source)
	lex := parser.NewPromiseLexer(input)
	lex.RemoveErrorListeners()
	tokens := lex.GetAllTokens()
	refs := findCatalogRefs(tokens, cat)
	if len(refs) == 0 {
		return source
	}
	bound := findLeadingUseBindings(tokens)
	var missing []string
	for name := range refs {
		if !bound[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return source
	}
	sort.Strings(missing)
	var b strings.Builder
	for _, name := range missing {
		b.WriteString("use ")
		b.WriteString(name)
		b.WriteString(";\n")
	}
	b.WriteString(source)
	return b.String()
}

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
		} else if args[i] == "-target" && i+1 < len(args) {
			target = args[i+1]
			i++
		} else if args[i] == "-time-phases" {
			timePhases = true
		} else {
			remaining = append(remaining, args[i])
		}
	}

	checkTargetFlag(target)

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

	if target == "" {
		target = codegen.HostTargetTriple()
	}

	// Build cache (T0857): repeated `promise exec` of the same source skips
	// compilation and execs a cached binary, mirroring `promise run`. The key is
	// computed from the raw trimmed source before any source transforms — those are
	// deterministic functions of (source, compiler) already covered by the key.
	unlock := module.LockBuildDirShared()
	defer unlock()

	cacheKey, cacheable := computeExecBinaryCacheKey(source, target)
	var cacheDir string
	if cacheable {
		cacheDir, _ = module.BuildCacheDir()
	}

	// Cache hit — exec directly, no compile.
	if cacheDir != "" {
		if cachedBin := module.LookupTestBinaryCache(cacheDir, cacheKey); cachedBin != "" {
			if os.Getenv("PROMISE_CACHE_DEBUG") != "" {
				fmt.Fprintf(os.Stderr, "[cache HIT] <exec> key=%s\n", cacheKey[:16])
			}
			executeExecBinary(target, cachedBin, timeout)
			return
		}
	}

	if os.Getenv("PROMISE_CACHE_DEBUG") != "" {
		if cacheable {
			fmt.Fprintf(os.Stderr, "[cache MISS] <exec> key=%s compiler=%s std=%s target=%s\n",
				cacheKey[:16], module.CompilerHash()[:16], cachedStdHash()[:16], target)
			if os.Getenv("PROMISE_CACHE_DEBUG") == "verbose" {
				fmt.Fprintln(os.Stderr, module.FormatCacheKeyInputs(
					"exec-binary <exec>", cacheKey, computeExecBinaryCacheInputs(source, target)))
			}
		} else {
			fmt.Fprintln(os.Stderr, "[cache SKIP] <exec> (not cacheable)")
		}
	}

	// Auto-inject `use <name>;` for referenced catalog modules (T0221).
	source = autoInjectCatalogUses(source)

	var compileStart time.Time
	if timePhases {
		compileStart = time.Now()
	}

	// Try parsing as-is first; if that fails, wrap in main!() and retry.
	tParse := time.Now()
	wrapOffset := 0
	file, ok := tryParseSourceString(source)
	if !ok {
		// Extract leading use declarations so they stay at file scope.
		usePrefix, body := extractUseDecls(source)
		wrappedSource := body
		if !strings.HasSuffix(wrappedSource, ";") && !strings.HasSuffix(wrappedSource, "}") {
			wrappedSource += ";"
		}
		wrappedSource = "main!() {\n" + wrappedSource + "\n}"
		if usePrefix != "" {
			wrappedSource = usePrefix + "\n" + wrappedSource
		}
		wrapOffset = 1 // for the main!() { line
		source = wrappedSource
		file = parseSourceString(source, wrapOffset)
	}
	timePhase("parse", time.Since(tParse), "")

	// Inject std as a glob import so all std symbols are available
	file = injectStdImport(file)
	file = injectGzipImportIfNeeded(file)

	tSema := time.Now()

	// Load local modules from use declarations
	filterTriple := target
	if filterTriple == "" {
		filterTriple = codegen.HostTargetTriple()
	}
	targetInfo := sema.ParseTargetInfo(filterTriple)
	moduleScopes, modInfos, depOrder, modTiming := loadModuleScopes("<exec>", file, targetInfo)

	// Semantic analysis
	tUserSema := time.Now()
	info, errs := sema.CheckWithTarget(file, moduleScopes, targetInfo)
	userSemaDur := time.Since(tUserSema)
	if modInfos != nil {
		info.ModuleInfos = modInfos
		info.ModuleOrder = depOrder
	}
	timePhase("sema", time.Since(tSema), "")
	if timePhases {
		if modTiming != nil {
			parseExtra := fmt.Sprintf("(%d files)", modTiming.files)
			if modTiming.parseCached {
				parseExtra += " (cached)"
			}
			timeSubPhase("mod parse", modTiming.parseTime, parseExtra)
			mt := modTiming.timings
			timeSubPhase("mod sema", modTiming.semaTime,
				fmt.Sprintf("(declare: %dms, define: %dms, check: %dms, verify: %dms)",
					mt.Declare.Milliseconds(), mt.Define.Milliseconds(),
					mt.Check.Milliseconds(), mt.Verify.Milliseconds()))
		}
		ut := info.Timings
		timeSubPhase("user sema", userSemaDur,
			fmt.Sprintf("(declare: %dms, define: %dms, check: %dms, verify: %dms)",
				ut.Declare.Milliseconds(), ut.Define.Milliseconds(),
				ut.Check.Milliseconds(), ut.Verify.Milliseconds()))
	}
	if len(errs) > 0 {
		printInlineErrors(source, errs, wrapOffset)
		os.Exit(1)
	}

	// Ownership analysis
	tOwner := time.Now()
	ownerErrs := ownership.Check(file, info)
	timePhase("ownership", time.Since(tOwner), "")
	if len(ownerErrs) > 0 {
		printInlineErrors(source, ownerErrs, wrapOffset)
		os.Exit(1)
	}

	// Code generation
	tCodegen := time.Now()
	result := codegen.CompileWithOptions(file, info, target, &codegen.CompileOptions{
		CachedInstances: lookupCachedInstances(info, target, "debug"),
		DebugAllocator:  true, // exec uses debug mode
	})
	timePhase("codegen", time.Since(tCodegen), "")

	// Compile and link to temp binary
	ext := binaryExtension(target)
	tmpOutput, err := os.CreateTemp("", "promise-exec-*"+ext)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating temp file: %v\n", err)
		os.Exit(1)
	}
	tmpOutput.Close()
	defer os.Remove(tmpOutput.Name())

	compileAndLink(result, tmpOutput.Name(), target, "", false) // exec always uses debug mode

	// Save the compiled binary to the cache for future runs (T0857).
	if cacheDir != "" {
		if err := module.SaveTestBinaryCache(cacheDir, cacheKey, tmpOutput.Name()); err == nil {
			module.SaveTestBinaryMeta(cacheDir, cacheKey, &module.CacheMeta{
				Kind:     module.CacheKindBinary,
				Name:     "<exec>",
				CacheKey: cacheKey,
			})
		}
	}

	if timePhases {
		timePhase("total", time.Since(compileStart), "")
	}

	executeExecBinary(target, tmpOutput.Name(), timeout)
}

// executeExecBinary runs a compiled `promise exec` binary with the current
// process's stdio wired through, applying the given execution timeout. It selects
// the correct runner for wasm/wasm-web targets and exits with the child's exit
// code on failure. Shared by the cache-hit and cache-miss paths so timeout and
// wasm-runner behavior stay identical.
func executeExecBinary(target, binPath string, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var cmd *exec.Cmd
	switch {
	case isWasmWebTarget(target):
		cmd = runWasmWeb(ctx, binPath)
	case isWasmTarget(target):
		cmd = exec.CommandContext(ctx, "wasmtime", binPath)
	default:
		cmd = exec.CommandContext(ctx, binPath)
	}
	isolateProcessGroup(cmd)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			fmt.Fprintf(os.Stderr, "\nTIMEOUT: execution exceeded %s timeout\n", timeout)
			os.Exit(1)
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(sanitizeExitCode(exitErr.ExitCode()))
		}
		fmt.Fprintf(os.Stderr, "error running: %v\n", err)
		os.Exit(1)
	}
}

// tryParseSourceString attempts to parse source as a complete Promise file.
// Returns the parsed file and true if parsing succeeds AND a main function exists.
// Returns nil and false otherwise (parse errors or no main function).
func tryParseSourceString(source string) (*ast.File, bool) {
	input := antlr.NewInputStream(source)

	el := &errorListener{source: source, silent: true}

	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	lexer.AddErrorListener(el)

	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := parser.NewPromiseParser(stream)
	p.RemoveErrorListeners()
	p.AddErrorListener(el)

	tree := p.CompilationUnit()

	if el.errors > 0 {
		return nil, false
	}

	file, errs := ast.Build("", tree)
	if len(errs) > 0 {
		return nil, false
	}

	// Check that the file has a main function — without one, the code
	// needs to be wrapped in main!().
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name == "main" {
			return file, true
		}
	}
	return nil, false
}

// parseSourceString parses Promise source code from a string.
// Uses inline error formatting with source context display.
func parseSourceString(source string, wrapOffset int) *ast.File {
	input := antlr.NewInputStream(source)

	el := &errorListener{source: source, wrapOffset: wrapOffset}

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
		printInlineErrors(source, errs, wrapOffset)
		os.Exit(1)
	}
	return file
}

// printInlineErrors formats errors with source context for inline execution.
func printInlineErrors(source string, errs []error, wrapOffset int) {
	lines := strings.Split(source, "\n")
	lineOffset := wrapOffset

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
		fmt.Fprintf(os.Stderr, "    %s\n", expandTabs(lines[srcIdx-1]))
	}
	line := lines[srcIdx]
	fmt.Fprintf(os.Stderr, "  > %s\n", expandTabs(line))
	if column >= 0 {
		visualCol := tabExpandedColumn(line, column)
		fmt.Fprintf(os.Stderr, "    %s^\n", strings.Repeat(" ", visualCol))
	}
}

// expandTabs replaces tab characters with spaces using 4-column tab stops.
func expandTabs(s string) string {
	if !strings.Contains(s, "\t") {
		return s
	}
	var buf strings.Builder
	col := 0
	for _, c := range s {
		if c == '\t' {
			spaces := 4 - (col % 4)
			buf.WriteString(strings.Repeat(" ", spaces))
			col += spaces
		} else {
			buf.WriteRune(c)
			col++
		}
	}
	return buf.String()
}

// tabExpandedColumn converts a character column to a visual column,
// accounting for tab characters that expand to 4-column tab stops.
func tabExpandedColumn(line string, charCol int) int {
	visual := 0
	for i := 0; i < charCol && i < len(line); i++ {
		if line[i] == '\t' {
			visual += 4 - (visual % 4)
		} else {
			visual++
		}
	}
	if charCol > len(line) {
		visual += charCol - len(line)
	}
	return visual
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
// filename is the fallback source path when an error has no position file
// (e.g. a top-level message). When errors carry per-file positions (project
// or module-test builds spanning multiple files), context is loaded from the
// per-error file so the snippet under the message stays accurate.
func printFileErrors(filename string, errs []error) {
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
		ctxFile := pos.File
		if ctxFile == "" {
			ctxFile = filename
		}
		if lines := readFileLines(ctxFile); lines != nil {
			printErrorContext(lines, pos.Line-1, pos.Column)
		}
	}
}

// --- Install ---

// runCatalog implements the `promise catalog` subcommand.
func runCatalog(args []string) {
	if len(args) == 0 {
		// Pure group: a bare invocation lists its subcommands to stdout, exit 0
		// (T1006).
		node, matched, _ := findNode([]string{"catalog"})
		printNodeHelp(os.Stdout, node, matched)
		return
	}

	switch args[0] {
	case "list":
		runCatalogList()
	default:
		fmt.Fprintf(os.Stderr, "unknown catalog subcommand: %s\n", args[0])
		helpHint(os.Stderr)
		os.Exit(1)
	}
}

// runCatalogList prints all available catalog modules.
func runCatalogList() {
	if len(embeddedCatalog) == 0 {
		fmt.Fprintln(os.Stderr, "error: no catalog available")
		os.Exit(1)
	}

	cat, err := module.ParseCatalog(embeddedCatalog)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: invalid catalog: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Promise catalog (epoch %s)\n\n", cat.Epoch)

	// Sort module names for stable output
	names := make([]string, 0, len(cat.Modules))
	for name := range cat.Modules {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		entry := cat.Modules[name]
		source := "embedded"
		if !entry.IsEmbedded() {
			source = entry.URL
		}
		fmt.Printf("  %-12s %s\n", name, source)
		if entry.Description != "" {
			fmt.Printf("  %-12s %s\n", "", entry.Description)
		}
	}

	fmt.Printf("\n%d modules\n", len(names))
}

// runInstall installs the Promise compiler to PROMISE_HOME (default: ~/.promise/).
// runInit creates a promise.toml in the current directory.
// runClean removes the build cache and optionally the global module cache.
func runClean(args []string) {
	global := false
	epochs := false
	for _, a := range args {
		switch a {
		case "-global":
			global = true
		case "-epochs":
			epochs = true
		default:
			fmt.Fprintf(os.Stderr, "usage: promise clean [-global] [-epochs]\n")
			os.Exit(1)
		}
	}

	// -epochs is an alias for `promise remove -all-except-active`.
	if epochs {
		runRemove([]string{"-all-except-active"})
	}

	// Serialize with concurrent test/build operations.
	unlock := module.LockBuildDirExclusive()
	defer unlock()

	// Clean build cache
	if err := module.CleanBuildCache(); err != nil {
		fmt.Fprintf(os.Stderr, "error cleaning build cache: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Cleaned build cache")

	if global {
		if err := module.CleanGlobalCache(); err != nil {
			fmt.Fprintf(os.Stderr, "error cleaning module cache: %v\n", err)
			os.Exit(1)
		}
		// Also clean extraction caches and stamp so next build re-extracts
		module.CleanEmbeddedModuleCache()
		module.CleanLLVMCache()
		module.CleanCRTCache()
		// Remove the compiler stamp so the next run re-populates
		if path, err := module.CompilerStampPath(); err == nil {
			os.Remove(path)
		}
		fmt.Println("Cleaned module cache")
	}
}

// runPin resolves a remote module ref to a full commit SHA and writes it to promise.toml.
func runPin(args []string) {
	if len(args) < 1 || len(args) > 2 {
		fmt.Fprintln(os.Stderr, "usage: promise package pin <url> [ref]")
		fmt.Fprintln(os.Stderr, "  ref: tag, branch, commit hash, or HEAD (default: HEAD)")
		os.Exit(1)
	}

	url := args[0]
	ref := "HEAD"
	if len(args) == 2 {
		ref = args[1]
	}

	// Find promise.toml
	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	cfg, err := module.FindConfig(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if cfg == nil {
		fmt.Fprintln(os.Stderr, "error: no promise.toml found (run 'promise init' first)")
		os.Exit(1)
	}

	// Resolve ref to full commit hash
	fmt.Fprintf(os.Stderr, "Resolving %s @ %s...\n", url, ref)
	commitHash, err := module.PinResolve(url, ref)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// Write to promise.toml
	tomlPath := filepath.Join(cfg.Dir, "promise.toml")
	if err := module.SetRequire(tomlPath, url, commitHash); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Pinned %s → %s\n", url, commitHash[:12])
}

// isBareName reports whether s is a bare module name (a catalog key like "json")
// rather than a git URL or local path. Catalog names carry no path separator,
// scheme, or drive colon, so the absence of "/" and ":" (and not being a local
// path) distinguishes them from `github.com/x/y`, `https://…`, and `../foo`.
func isBareName(s string) bool {
	return !strings.Contains(s, "/") && !strings.Contains(s, ":") && !module.IsLocalPath(s)
}

// runAdd implements the `promise package add <name|url> [ref]` subcommand.
// If the first argument matches a catalog name, resolves to its URL.
// Otherwise treats it as a raw git URL. Writes to promise.toml.
func runAdd(args []string) {
	if len(args) < 1 || len(args) > 2 {
		fmt.Fprintln(os.Stderr, "usage: promise package add <name|url> [ref]")
		fmt.Fprintln(os.Stderr, "  name: catalog module name (e.g., json) or git URL")
		fmt.Fprintln(os.Stderr, "  ref:  tag, branch, commit hash, or HEAD (default: HEAD)")
		os.Exit(1)
	}

	nameOrURL := args[0]
	// An explicit ref disables epoch-aware tag resolution (the user pinned it);
	// with no ref we resolve the epoch-appropriate `epoch-*` tag (§9.8).
	explicitRef := ""
	if len(args) == 2 {
		explicitRef = args[1]
	}

	// Find promise.toml
	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	cfg, err := module.FindConfig(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if cfg == nil {
		fmt.Fprintln(os.Stderr, "error: no promise.toml found (run 'promise init' first)")
		os.Exit(1)
	}
	if cfg.Epoch == "" {
		fmt.Fprintln(os.Stderr, "error: promise.toml [module] must declare an epoch to resolve modules")
		os.Exit(1)
	}

	// Check catalog (skip if no embedded catalog — treat argument as raw URL)
	url := nameOrURL
	label := nameOrURL
	resolvedByCatalog := false
	if len(embeddedCatalog) > 0 {
		cat, err := module.ParseCatalog(embeddedCatalog)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: invalid catalog: %v\n", err)
			os.Exit(1)
		}
		if entry := cat.Lookup(nameOrURL); entry != nil {
			if entry.IsEmbedded() {
				fmt.Printf("module '%s' is built-in — use it with: use %s;\n", nameOrURL, nameOrURL)
				return
			}
			url = entry.URL
			label = fmt.Sprintf("%s (%s)", nameOrURL, url)
			resolvedByCatalog = true
		}
	}

	// Community catalog (§9.9 step 4): a bare NAME not found in the embedded
	// catalog resolves through the living community catalog's per-epoch compat
	// index. The CI-recorded verdict is trusted directly — no local test run
	// (§9.9). An explicit URL or ref disambiguates to the ad-hoc path and bypasses
	// this; the embedded catalog already shadowed community on a name collision.
	if !resolvedByCatalog && explicitRef == "" && isBareName(nameOrURL) {
		fmt.Fprintf(os.Stderr, "Resolving %s via community catalog for epoch %s...\n", nameOrURL, cfg.Epoch)
		curl, ccommit, found, cerr := resolveCommunity(nameOrURL, cfg.Epoch)
		if cerr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", cerr)
			os.Exit(1)
		}
		if found {
			tomlPath := filepath.Join(cfg.Dir, "promise.toml")
			if err := module.SetRequire(tomlPath, curl, ccommit); err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("Added %s (community: %s) → %s\n", nameOrURL, curl, shortCommit(ccommit))
			return
		}
		// Not listed in any catalog → not name-addressable (§9.9 step 5).
		fmt.Fprintf(os.Stderr, "error: '%s' is not a first-party or community catalog module\n", nameOrURL)
		fmt.Fprintln(os.Stderr, "  hint: add it by git URL — promise package add <git-url> [ref]")
		os.Exit(1)
	}

	// Verification runs under the project's epoch — use this compiler only if it
	// implements that epoch, otherwise direct the user to switch.
	compilerBin, err := projectEpochCompiler(cfg.Epoch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// Resolve an epoch-appropriate commit and verify it under the project epoch
	// (§9.8/§9.9). On no compatible version this returns the §9.10 gate error.
	if explicitRef != "" {
		fmt.Fprintf(os.Stderr, "Resolving %s @ %s (epoch %s)...\n", label, explicitRef, cfg.Epoch)
	} else {
		fmt.Fprintf(os.Stderr, "Resolving %s for epoch %s...\n", label, cfg.Epoch)
	}
	warn := func(msg string) { fmt.Fprintf(os.Stderr, "  %s\n", msg) }
	commitHash, err := resolveEpochAware(compilerBin, cfg.Epoch, label, url, explicitRef, warn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// Write to promise.toml
	tomlPath := filepath.Join(cfg.Dir, "promise.toml")
	if err := module.SetRequire(tomlPath, url, commitHash); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Added %s → %s\n", label, shortCommit(commitHash))
}

// runSearch implements the `promise package search <keyword>` subcommand.
// Searches the embedded catalog by module name and description.
func runSearch(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: promise package search <keyword>")
		os.Exit(1)
	}

	keyword := strings.ToLower(args[0])

	if len(embeddedCatalog) == 0 {
		fmt.Fprintln(os.Stderr, "error: no catalog available")
		os.Exit(1)
	}
	cat, err := module.ParseCatalog(embeddedCatalog)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: invalid catalog: %v\n", err)
		os.Exit(1)
	}

	// Collect matches
	type match struct {
		name  string
		entry *module.CatalogEntry
	}
	var matches []match
	for name, entry := range cat.Modules {
		if strings.Contains(strings.ToLower(name), keyword) ||
			strings.Contains(strings.ToLower(entry.Description), keyword) {
			matches = append(matches, match{name, entry})
		}
	}

	if len(matches) == 0 {
		fmt.Fprintf(os.Stderr, "No modules matching '%s'\n", args[0])
		return
	}

	// Sort alphabetically
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].name < matches[j].name
	})

	for _, m := range matches {
		source := "embedded"
		if !m.entry.IsEmbedded() {
			source = m.entry.URL
		}
		fmt.Printf("  %-12s %s\n", m.name, source)
		if m.entry.Description != "" {
			fmt.Printf("  %-12s %s\n", "", m.entry.Description)
		}
	}

	fmt.Printf("\n%d matching modules\n", len(matches))
}

// runPackage dispatches `promise package` subcommands.
func runPackage(args []string) {
	if len(args) == 0 {
		// Pure group: a bare invocation lists its subcommands to stdout, exit 0
		// (T1006).
		node, matched, _ := findNode([]string{"package"})
		printNodeHelp(os.Stdout, node, matched)
		return
	}
	switch args[0] {
	case "add":
		runAdd(args[1:])
	case "remove":
		runPackageRemove(args[1:])
	case "update":
		runPkgUpdate(args[1:])
	case "search":
		runSearch(args[1:])
	case "pin":
		runPin(args[1:])
	case "check-upgrade":
		runPackageCheckUpgrade(args[1:])
	case "check-epoch":
		runPackageCheckEpoch(args[1:])
	case "build-index":
		runPackageBuildIndex(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown package subcommand: %s\n", args[0])
		helpHint(os.Stderr)
		os.Exit(1)
	}
}

// runLegacyPackageAlias routes deprecated top-level aliases (pkg, add, search, pin)
// to their `promise package` equivalents for one-release backward compatibility.
func runLegacyPackageAlias(cmd string, args []string) {
	switch cmd {
	case "pkg":
		if len(args) > 0 && args[0] == "update" {
			runPkgUpdate(args[1:])
		} else {
			runPackage(args)
		}
	case "add":
		runAdd(args)
	case "search":
		runSearch(args)
	case "pin":
		runPin(args)
	}
}

// runPackageRemove implements `promise package remove <url>`.
func runPackageRemove(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: promise package remove <url>")
		os.Exit(1)
	}
	url := args[0]

	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	cfg, err := module.FindConfig(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if cfg == nil {
		fmt.Fprintln(os.Stderr, "error: no promise.toml found (run 'promise init' first)")
		os.Exit(1)
	}

	found := false
	for u := range cfg.Require {
		if module.NormalizeURL(u) == module.NormalizeURL(url) {
			found = true
			break
		}
	}
	if !found {
		fmt.Fprintf(os.Stderr, "error: no [require] entry matching '%s'\n", url)
		os.Exit(1)
	}

	tomlPath := filepath.Join(cfg.Dir, "promise.toml")
	if err := module.RemoveRequire(tomlPath, url); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Removed %s\n", url)
}

// runPkgUpdate updates git-dependency [require]/[require.NAME] pins in
// promise.toml. Previously the body of `promise update`; moved under
// `promise pkg update` (T0770), now available as `promise package update`.
func runPkgUpdate(args []string) {
	if len(args) > 1 {
		fmt.Fprintln(os.Stderr, "usage: promise package update [url]")
		fmt.Fprintln(os.Stderr, "  With no arguments, updates all [require] entries.")
		fmt.Fprintln(os.Stderr, "  With a name or URL, updates only that entry.")
		os.Exit(1)
	}

	// Find promise.toml
	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	cfg, err := module.FindConfig(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if cfg == nil {
		fmt.Fprintln(os.Stderr, "error: no promise.toml found (run 'promise init' first)")
		os.Exit(1)
	}
	if cfg.Epoch == "" {
		fmt.Fprintln(os.Stderr, "error: promise.toml [module] must declare an epoch to resolve modules")
		os.Exit(1)
	}

	tomlPath := filepath.Join(cfg.Dir, "promise.toml")

	// Build list of entries to update
	type updateEntry struct {
		label         string // display label
		url           string // git URL
		currentCommit string // current pinned commit
		named         bool   // true if [require.NAME] entry
		name          string // NAME for named entries
	}
	var entries []updateEntry

	if len(args) == 1 {
		target := args[0]
		found := false

		// Check URL-keyed entries
		normalizedTarget := module.NormalizeURL(target)
		for url, commit := range cfg.Require {
			if module.NormalizeURL(url) == normalizedTarget || url == target {
				entries = append(entries, updateEntry{url, url, commit, false, ""})
				found = true
				break
			}
		}

		// Check named entries
		if !found {
			for name, entry := range cfg.NamedRequire {
				if name == target || module.NormalizeURL(entry.URL) == normalizedTarget {
					entries = append(entries, updateEntry{name, entry.URL, entry.Commit, true, name})
					found = true
					break
				}
			}
		}

		if !found {
			fmt.Fprintf(os.Stderr, "error: no [require] entry matching '%s'\n", target)
			os.Exit(1)
		}
	} else {
		// Update all
		for url, commit := range cfg.Require {
			entries = append(entries, updateEntry{url, url, commit, false, ""})
		}
		for name, entry := range cfg.NamedRequire {
			entries = append(entries, updateEntry{name, entry.URL, entry.Commit, true, name})
		}
	}

	if len(entries) == 0 {
		fmt.Println("No [require] entries to update.")
		return
	}

	// Sort for stable output
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].label < entries[j].label
	})

	// Re-resolution re-runs the epoch-aware tag pick + verification (§9.8 step 3:
	// moving a tag upstream never changes an existing build until `update`), so it
	// needs the project-epoch compiler. Resolved lazily on the first git entry —
	// an update over only non-git (sha256) sources never needs it.
	var compilerBin string
	ensureCompiler := func() bool {
		if compilerBin != "" {
			return true
		}
		b, err := projectEpochCompiler(cfg.Epoch)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		compilerBin = b
		return true
	}

	updated := 0
	for _, e := range entries {
		// Skip SHA256-only (non-git) entries — PinResolve requires a git URL
		if e.named && e.currentCommit == "" {
			fmt.Printf("  %s: skipped (non-git source)\n", e.label)
			continue
		}

		// Community modules (§9.9) re-resolve through the FRESH community index
		// (authoritative, no local test run) rather than the generic epoch-tag
		// walk-back. Falls through to the engine path if the URL is no longer
		// listed in the community catalog.
		var newCommit string
		if module.ModuleTier(e.url) == module.TierCommunity {
			fmt.Fprintf(os.Stderr, "Checking %s (community catalog)...\n", e.label)
			c, found, cerr := resolveCommunityByURL(e.url, cfg.Epoch)
			if cerr != nil {
				fmt.Fprintf(os.Stderr, "  %s: error: %v\n", e.label, cerr)
				continue
			}
			if found {
				newCommit = c
			}
		}
		if newCommit == "" {
			ensureCompiler()
			fmt.Fprintf(os.Stderr, "Checking %s...\n", e.label)
			warn := func(msg string) { fmt.Fprintf(os.Stderr, "  %s\n", msg) }
			c, err := resolveEpochAware(compilerBin, cfg.Epoch, e.label, e.url, "", warn)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  %s: error: %v\n", e.label, err)
				continue
			}
			newCommit = c
		}

		if newCommit == e.currentCommit {
			fmt.Printf("  %s: already up to date\n", e.label)
			continue
		}

		if e.named {
			if err := module.SetNamedRequireCommit(tomlPath, e.name, newCommit); err != nil {
				fmt.Fprintf(os.Stderr, "  %s: error: %v\n", e.label, err)
				continue
			}
		} else {
			if err := module.SetRequire(tomlPath, e.url, newCommit); err != nil {
				fmt.Fprintf(os.Stderr, "  %s: error: %v\n", e.label, err)
				continue
			}
		}

		oldShort := e.currentCommit
		if len(oldShort) > 12 {
			oldShort = oldShort[:12]
		}
		fmt.Printf("  %s: %s → %s\n", e.label, oldShort, newCommit[:12])
		updated++
	}

	fmt.Printf("\nUpdated %d of %d dependencies\n", updated, len(entries))
}

func runInit(args []string) {
	// Derive the epoch from the running compiler so a fresh scaffold always
	// targets an epoch this toolchain can actually build (T0972).
	defaultEpoch, err := module.CompilerEpoch(embeddedCatalog)
	if err != nil || defaultEpoch == "" {
		// Should never trigger in a real build (the catalog is always embedded).
		defaultEpoch = "2026.1"
	}

	// Parse --force flag and optional target directory
	force := false
	targetDir := ""
	for _, arg := range args {
		if arg == "-force" || arg == "--force" {
			force = true
		} else if !strings.HasPrefix(arg, "-") && targetDir == "" {
			targetDir = arg
		}
	}
	if targetDir == "" {
		targetDir = "."
	}

	// Resolve to absolute path
	absDir, err := filepath.Abs(targetDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// Create directory if it doesn't exist
	if info, err := os.Stat(absDir); os.IsNotExist(err) {
		if err := os.MkdirAll(absDir, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "error creating directory: %v\n", err)
			os.Exit(1)
		}
	} else if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	} else if !info.IsDir() {
		fmt.Fprintf(os.Stderr, "error: %s is not a directory\n", absDir)
		os.Exit(1)
	} else if !force {
		// Check if directory is non-empty (ignoring dotfiles)
		entries, err := os.ReadDir(absDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), ".") {
				fmt.Fprintf(os.Stderr, "error: directory %s is not empty (use --force to override)\n", absDir)
				os.Exit(1)
			}
		}
	}

	j := func(file string) string { return filepath.Join(absDir, file) }
	name := filepath.Base(absDir)

	if _, err := os.Stat(j("promise.toml")); err == nil {
		fmt.Fprintln(os.Stderr, "promise.toml already exists")
		os.Exit(1)
	}

	content := fmt.Sprintf("[module]\nname = %q\nepoch = %q\n", name, defaultEpoch)
	if err := os.WriteFile(j("promise.toml"), []byte(content), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "error writing promise.toml: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Created promise.toml (module: %s, epoch: %s)\n", name, defaultEpoch)

	// Generate main.pr if it doesn't exist
	if _, err := os.Stat(j("main.pr")); err != nil {
		mainContent := "use io;\n" +
			"use os;\n" +
			"\n" +
			"greet(string name) string `public `doc(\"Returns a friendly greeting for the given name.\") {\n" +
			"    return \"Hello, {name}!\";\n" +
			"}\n" +
			"\n" +
			`main!() {
    print_line(greet("Promise"));

    // Module-qualified access, auto-propagated errors (!), string interpolation
    cwd := os.working_dir;
    print_line("Working directory: {cwd}");

    // Failable call with ? catch — returns fallback on error
    names := io.Dir.list(cwd);
    print_line("Files: {names.len}");

    safe := io.Dir.list("/nonexistent") ? { string[](); };
    print_line("Safe: {safe.len} items");
}
`
		if err := os.WriteFile(j("main.pr"), []byte(mainContent), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "error writing main.pr: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Created main.pr")
	}

	// Generate CLAUDE.md if it doesn't exist
	if _, err := os.Stat(j("CLAUDE.md")); err != nil {
		claudeContent := `# ` + name + `

Promise project. Use ` + "`promise guide`" + ` for the full language reference.

## Quick Start

` + "```" + `bash
promise run                     # build and run
promise build                   # build only
promise test                    # run tests
promise exec 'print_line("hi")' # run a one-liner
promise doc <module>            # show module API docs
` + "```" + `

## Error Handling

` + "```" + `
main!() {   # ! marks main failable (can return error)
  f();      # unhandled errors raise to caller
  f()?^;    # explicit propagation - raise error to caller
  f()?!;    # panic on error
  v := f() ? { fallback(); };  # catch with recovery block
}
` + "```" + `

## Module Rules

- Import with ` + "`use io;`" + ` — access as ` + "`io.File`" + `, ` + "`io.Dir`" + ` (always module-qualified)
- Standard library (` + "`std`" + `) is auto-imported — ` + "`print_line`" + `, ` + "`Vector`" + `, ` + "`Map`" + `, etc. need no prefix

## Available Modules

| Module | Purpose | Docs |
|--------|---------|------|
| ` + "`io`" + ` | File I/O, buffered readers/writers, directories | ` + "`promise doc io`" + ` |
| ` + "`os`" + ` | Environment, process execution, signals | ` + "`promise doc os`" + ` |
| ` + "`json`" + ` | JSON encode/decode, JsonValue | ` + "`promise doc json`" + ` |
| ` + "`path`" + ` | Path joining, dir/base/ext extraction | ` + "`promise doc path`" + ` |
| ` + "`math`" + ` | Extended math functions | ` + "`promise doc math`" + ` |
| ` + "`strings`" + ` | Extended string utilities | ` + "`promise doc strings`" + ` |
| ` + "`time`" + ` | Extended time utilities | ` + "`promise doc time`" + ` |
| ` + "`http`" + ` | HTTP client | ` + "`promise doc http`" + ` |
`
		if err := os.WriteFile(j("CLAUDE.md"), []byte(claudeContent), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "error writing CLAUDE.md: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Created CLAUDE.md")
	}
}

// runInstallEpoch fetches + stages a specific epoch WITHOUT activating it
// (presence only; mirrors rustup `toolchain install`). This is the recovery
// path for a project pinning an epoch that is not present on disk: building a
// pinned project needs presence, not activation, so this leaves the active
// pointer untouched and does not perturb other unpinned projects.
func runInstallEpoch(epoch string) {
	if err := ensureEpochPresent(epoch); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Installed epoch %s (active epoch unchanged)\n", epoch)
}

func runInstall(args []string) {
	// Overload: `promise install <epoch>` fetches + stages an epoch without
	// activating it (presence-only). The no-arg bootstrap path below — which
	// installs THIS binary's embedded epoch and activates it — is the critical
	// installation primitive and stays byte-for-byte unchanged; the positional
	// guard can only trigger on a single non-flag argument.
	if len(args) == 1 && !strings.HasPrefix(args[0], "-") {
		runInstallEpoch(args[0])
		return
	}

	// Parse flags:
	//   -dev                  install into epochs/dev/ instead of epochs/<epoch>/
	//   --no-fetch-toolchain  skip the install-time host-toolchain pre-fetch
	//                         (the install script's --thin); the toolchain then
	//                         downloads lazily on the first compile. A full
	//                         binary embeds the toolchain, so the pre-fetch is a
	//                         no-network view build either way.
	//   --no-modify-path      do not add <PROMISE_HOME>\bin to the User PATH
	//                         (Windows only; no-op elsewhere). Also honored via
	//                         the PROMISE_NO_MODIFY_PATH env var so the install
	//                         scripts and the install gate can opt out without a
	//                         flag (T0863/T0864).
	devMode := false
	fetchToolchain := true
	modifyPath := os.Getenv("PROMISE_NO_MODIFY_PATH") == ""
	for _, arg := range args {
		switch arg {
		case "-dev":
			devMode = true
		case "--no-fetch-toolchain", "-no-fetch-toolchain":
			fetchToolchain = false
		case "--no-modify-path", "-no-modify-path":
			modifyPath = false
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\nusage: promise install [-dev] [--no-fetch-toolchain] [--no-modify-path]\n", arg)
			os.Exit(1)
		}
	}

	promiseDir, err := module.PromiseHome()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot determine Promise home: %v\n", err)
		os.Exit(1)
	}

	// Determine epoch: --dev overrides to "dev", otherwise read from embedded catalog.
	var epoch string
	if devMode {
		epoch = "dev"
	} else {
		epoch, err = module.CompilerEpoch(embeddedCatalog)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: cannot determine compiler epoch: %v\n", err)
			os.Exit(1)
		}
	}

	epochDir, err := module.EpochDir(epoch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot determine epoch directory: %v\n", err)
		os.Exit(1)
	}

	binaryName := "promise"
	if runtime.GOOS == "windows" {
		binaryName = "promise.exe"
	}

	// Epoch subdirectories.
	epochBinDir := filepath.Join(epochDir, "bin")
	epochLibDir := filepath.Join(epochDir, "lib")
	epochStdDest := filepath.Join(epochLibDir, "std")
	epochCacheDir := filepath.Join(epochDir, "cache", "build")

	// Launcher stub directory at top-level ~/.promise/bin/.
	stubBinDir := filepath.Join(promiseDir, "bin")

	// Create all directories.
	dirs := []string{epochBinDir, epochStdDest, epochCacheDir, stubBinDir}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "error creating %s: %v\n", dir, err)
			os.Exit(1)
		}
	}

	// Copy binary to epoch directory.
	execPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot determine executable path: %v\n", err)
		os.Exit(1)
	}
	copyFile(execPath, filepath.Join(epochBinDir, binaryName), 0755)

	// Install the launcher stub at ~/.promise/bin/promise (T0770). The stub is
	// the Promise-built launcher (§2.5) that exec-replaces into the resolved
	// epoch's compiler. Update it FORWARD-ONLY: replace it only when this
	// installer's embedded stub is newer than the installed one, decided by a
	// plain read of the sidecar — never by executing the stub (§2.4 step 4).
	stubLabel := "launcher (dev: compiler copy)"
	stubPath := filepath.Join(stubBinDir, binaryName)
	if hasEmbeddedStub {
		stubLabel = "stub"
		if stubVersion > readInstalledStubVersion(stubBinDir) {
			if err := writeStubAndSidecar(stubBinDir, binaryName); err != nil {
				fmt.Fprintf(os.Stderr, "error installing stub: %v\n", err)
				os.Exit(1)
			}
		}
	} else {
		// Dev fallback: no Promise stub is embedded (it is a release-time
		// per-target artifact, T0773). Place the compiler itself at the launcher
		// path so PATH works — with the shim retired, a directly-run compiler
		// just runs as the compiler. Refresh the copy when the launcher is absent
		// or is itself a dev launcher (sidecar version 0) so PATH does not diverge
		// from the freshly installed epoch binary; but NEVER overwrite a real stub
		// (version > 0) — that would downgrade it to a non-dispatching compiler.
		_, statErr := os.Stat(stubPath)
		if os.IsNotExist(statErr) || readInstalledStubVersion(stubBinDir) == 0 {
			copyFile(execPath, stubPath, 0755)
			_ = writeFileAtomic(filepath.Join(stubBinDir, stubVersionSidecar), []byte("0\n"), 0644)
			fmt.Fprintln(os.Stderr, "note: dev build has no Promise stub; installed the compiler at the launcher path")
		}
	}

	// Extract embedded std files.
	extractEmbedded(embeddedModules, "resources/modules/std", epochStdDest)

	// Extract embedded catalog modules (io, json, path, etc.).
	cat, err := module.ParseCatalog(embeddedCatalog)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error parsing catalog: %v\n", err)
		os.Exit(1)
	}
	for name, entry := range cat.Modules {
		if name == "std" || !entry.IsEmbedded() {
			continue
		}
		modDest := filepath.Join(epochLibDir, "modules", name)
		if err := os.MkdirAll(modDest, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "error creating %s: %v\n", modDest, err)
			os.Exit(1)
		}
		extractEmbedded(embeddedModules, "resources/modules/"+name, modDest)
	}

	// Extract embedded musl CRT into the epoch lib dir (Linux). musl has no
	// public blob host yet (T0530), so it stays embedded-delivered; the CAS
	// musl path (resolveMuslCRTView) activates once the manifest carries musl
	// entries.
	if hasEmbeddedMuslCRT {
		arch := "x86_64-linux-musl"
		if runtime.GOARCH == "arm64" {
			arch = "aarch64-linux-musl"
		}
		crtDest := filepath.Join(epochLibDir, "crt", arch)
		if err := os.MkdirAll(crtDest, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "error creating %s: %v\n", crtDest, err)
			os.Exit(1)
		}
		extractEmbedded(embeddedMuslCRT, "resources/crt/"+arch, crtDest)
	}

	// Stage heavy dependencies into the content-addressed store and record the
	// epoch's blob references (T0769, §2.4 step 3 / §4.4). A full-variant binary
	// (hasEmbeddedLLVM) unpacks its bundled LLVM blobs into the shared CAS so the
	// host workflow runs offline; a thin binary stages nothing and fetches on
	// first use. Either way blobs.refs is derived from the embedded manifest
	// without executing any epoch binary, so GC (T0771) can compute roots from
	// epochs/*/blobs.refs alone.
	manifest, mErr := loadEmbeddedManifest()
	if mErr != nil {
		fmt.Fprintf(os.Stderr, "error: cannot parse embedded manifest: %v\n", mErr)
		os.Exit(1)
	}
	store, sErr := blobstore.NewStore()
	if sErr != nil {
		fmt.Fprintf(os.Stderr, "error: cannot open dependency cache: %v\n", sErr)
		os.Exit(1)
	}
	// Hold the CAS lock across blob staging + the blobs.refs write so a concurrent
	// gc/remove (T0771) — which holds the same lock — can never observe this
	// epoch's staged-but-not-yet-referenced blobs and sweep them out (§4.4). It is
	// released immediately after WriteEpochRefs: prefetchHostToolchain below
	// re-acquires the lock per-blob via the resolver, and a same-process flock on
	// a second fd would self-deadlock. Best-effort like runRemove — a lock syscall
	// failure (not contention; Lock blocks on contention) must not abort install,
	// since the gc/remove fail-safe still keeps a half-installed epoch's blobs.
	var casUnlock func()
	if unlock, lerr := store.Lock("install " + epoch); lerr == nil {
		casUnlock = unlock
	}
	stagedBlobs := false
	if hasEmbeddedLLVM {
		if err := stageEmbeddedLLVMBlobs(store); err != nil {
			if casUnlock != nil {
				casUnlock()
			}
			fmt.Fprintf(os.Stderr, "error staging LLVM blobs: %v\n", err)
			os.Exit(1)
		}
		stagedBlobs = true
	}
	if err := blobstore.WriteEpochRefs(epochDir, manifest); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write blobs.refs: %v\n", err)
	}
	if casUnlock != nil {
		casUnlock()
	}

	// Write active epoch file.
	if err := module.WriteActiveEpoch(epoch); err != nil {
		fmt.Fprintf(os.Stderr, "error writing active epoch: %v\n", err)
		os.Exit(1)
	}

	// Pre-fetch the host LLVM toolchain as part of setup (unless --thin /
	// --no-fetch-toolchain, or a -dev install which shouldn't reach out). This
	// moves the one-time multi-minute download here — where progress is expected
	// — instead of ambushing the user on their first `promise exec`. A full
	// binary already staged its blobs above, so this is just a fast view build.
	var toolchainView string
	if fetchToolchain && !devMode {
		toolchainView = prefetchHostToolchain()
	}

	fmt.Printf("Installed Promise epoch %s to %s\n", epoch, epochDir)
	fmt.Printf("  binary:  %s\n", filepath.Join(epochBinDir, binaryName))
	fmt.Printf("  %-7s %s\n", stubLabel+":", stubPath)
	fmt.Printf("  std:     %s\n", epochStdDest)
	fmt.Printf("  modules: %s\n", filepath.Join(epochLibDir, "modules"))
	if stagedBlobs {
		fmt.Printf("  blobs:   %s\n", filepath.Join(store.Root(), "blobs", "sha256"))
	}
	if toolchainView != "" {
		fmt.Printf("  toolchain: %s\n", toolchainView)
	}
	fmt.Printf("  refs:    %s\n", filepath.Join(epochDir, "blobs.refs"))
	fmt.Printf("  cache:   %s\n", epochCacheDir)
	fmt.Printf("  active:  %s\n", epoch)
	if runtime.GOOS == "windows" {
		// Add <PROMISE_HOME>\bin to the User PATH ourselves (registry, idempotent)
		// unless the user opted out. The previous advice — setx PATH "%PATH%;..." —
		// is destructive in both PowerShell and cmd.exe (T0863); addToUserPath does
		// it safely, and printWindowsPathHint shows the correct manual command.
		if modifyPath {
			switch changed, err := addToUserPath(stubBinDir); {
			case err != nil:
				fmt.Fprintf(os.Stderr, "\nwarning: could not update your User PATH automatically: %v\n", err)
				printWindowsPathHint(stubBinDir)
			case changed:
				fmt.Printf("\nAdded %s to your User PATH.\n", stubBinDir)
				fmt.Printf("Open a new terminal (or fully quit and reopen VS Code — Reload Window is not enough) for it to take effect.\n")
			default:
				fmt.Printf("\n%s is already on your User PATH.\n", stubBinDir)
			}
		} else {
			fmt.Printf("\nSkipped PATH setup (--no-modify-path / PROMISE_NO_MODIFY_PATH set).\n")
			printWindowsPathHint(stubBinDir)
		}
	} else {
		fmt.Printf("\nAdd to your shell profile:\n\n")
		fmt.Printf("  export PATH=\"%s:$PATH\"\n", stubBinDir)
	}
}

// runEpochs lists installed epochs, marking the active one.
func runEpochs() {
	epochs, err := module.InstalledEpochs()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error listing epochs: %v\n", err)
		os.Exit(1)
	}
	if len(epochs) == 0 {
		fmt.Println("No epochs installed.")
		return
	}

	active, _ := module.ActiveEpoch()

	var totalSize, totalCache int64
	for _, ep := range epochs {
		marker := " "
		suffix := ""
		if ep == active {
			marker = "*"
			suffix = "  (active)"
		}
		epochDir, err := module.EpochDir(ep)
		if err != nil {
			fmt.Printf("%s %s\n", marker, ep)
			continue
		}
		size := dirSize(epochDir)
		cacheSize := dirSize(filepath.Join(epochDir, "cache"))
		totalSize += size
		totalCache += cacheSize
		if cacheSize > 0 {
			fmt.Printf("%s %-12s %-8s (%s cache)%s\n", marker, ep, formatSize(size), formatSize(cacheSize), suffix)
		} else {
			fmt.Printf("%s %-12s %s%s\n", marker, ep, formatSize(size), suffix)
		}
	}

	fmt.Printf("\n%d epoch(s), %s", len(epochs), formatSize(totalSize))
	if totalCache > 0 {
		fmt.Printf(" (%s cache)", formatSize(totalCache))
	}
	fmt.Println()
}

// ensureEpochPresent makes <epoch> present on disk WITHOUT changing the active
// pointer. If the epoch's compiler binary already exists it is a no-op;
// otherwise the epoch is downloaded from releases and staged into
// epochs/<epoch>/. This is the shared install-half behind both `promise install
// <epoch>` (presence only) and `promise use <epoch>` (presence + activate);
// keeping it in one place makes `use` exactly `install` plus a WriteActiveEpoch.
func ensureEpochPresent(epoch string) error {
	// "next" is a rolling release *channel*, not a concrete epoch (T0825).
	// Following it installs whatever YYYY.N epoch the next branch currently
	// carries — the downloaded binary's `install` activates that real epoch, and
	// there is no epochs/next/ install directory. Staging the literal "next"
	// here would leave a dangling install directory at a non-existent epoch, so
	// direct the user to the channel command instead.
	if epoch == module.ChannelNext {
		return fmt.Errorf("%q is a release channel, not an epoch.\n"+
			"  To follow the next pre-release stream: promise update channel next", epoch)
	}

	epochDir, err := module.EpochDir(epoch)
	if err != nil {
		return err
	}
	binPath := filepath.Join(epochDir, "bin", "promise")
	if runtime.GOOS == "windows" {
		binPath += ".exe"
	}
	if _, err := os.Stat(binPath); err == nil {
		return nil // already present — no-op
	}

	// Not installed — download it from releases on demand (T0825). The child
	// `install` stages it into epochs/<epoch>/ AND writes the active pointer as
	// a side effect. Snapshot + restore the active pointer around the download
	// so presence-only installs never change the active epoch; `use` re-affirms
	// its own activation afterwards via WriteActiveEpoch.
	cfg := resolveSyncConfig()
	fmt.Fprintf(os.Stderr, "epoch %s is not installed; downloading from %s...\n", epoch, cfg.describe())
	release, derr := findSpecificRelease(cfg, epoch)
	if derr != nil {
		return derr
	}
	// Snapshot the active pointer BEFORE the download. If it can't be read
	// (a genuine I/O error, not "absent"), abort here rather than risk wiping
	// the pointer after the child install writes it — failing before the
	// download leaves nothing to undo.
	prevActive, hadActive, aerr := module.ActiveEpochRaw()
	if aerr != nil {
		return aerr
	}
	if derr := downloadAndInstall(release, epoch); derr != nil {
		return derr
	}
	if hadActive {
		return module.WriteActiveEpoch(prevActive)
	}
	return module.ClearActiveEpoch()
}

// runUse activates an epoch: makes it present (downloading on demand) and then
// sets it as the active epoch.
func runUse(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: promise use <epoch>")
		os.Exit(1)
	}
	epoch := args[0]

	if err := ensureEpochPresent(epoch); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if err := module.WriteActiveEpoch(epoch); err != nil {
		fmt.Fprintf(os.Stderr, "error writing active epoch: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Active epoch set to %s\n", epoch)
}

// runRemove removes one or more installed epochs.
func runRemove(args []string) {
	force := false
	allExceptActive := false
	var targets []string
	for _, arg := range args {
		switch arg {
		case "-force":
			force = true
		case "-all-except-active":
			allExceptActive = true
		default:
			if strings.HasPrefix(arg, "-") {
				fmt.Fprintf(os.Stderr, "unknown flag: %s\nusage: promise remove <epoch> [-force]\n       promise remove -all-except-active\n", arg)
				os.Exit(1)
			}
			targets = append(targets, arg)
		}
	}

	if !allExceptActive && len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "usage: promise remove <epoch> [-force]\n       promise remove -all-except-active")
		os.Exit(1)
	}

	active, _ := module.ActiveEpoch()

	if allExceptActive {
		epochs, err := module.InstalledEpochs()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error listing epochs: %v\n", err)
			os.Exit(1)
		}
		for _, ep := range epochs {
			if ep == active {
				continue
			}
			targets = append(targets, ep)
		}
		if len(targets) == 0 {
			fmt.Println("No non-active epochs to remove.")
			return
		}
	}

	// Hold the CAS lock across removal + the reclaim sweep so a concurrent
	// install/fetch can't race the live-set computation (§4.4). Lock failure is
	// non-fatal: removal still proceeds (rm -rf needs no lock); only the
	// exclusive-blob reclaim is skipped.
	var store *blobstore.Store
	if s, serr := blobstore.NewStore(); serr == nil {
		if unlock, lerr := s.Lock("remove"); lerr == nil {
			store = s
			defer unlock()
		}
	}

	for _, epoch := range targets {
		if epoch == active && !force {
			fmt.Fprintf(os.Stderr, "error: %q is the active epoch. Use -force to remove it.\n", epoch)
			os.Exit(1)
		}
		epochDir, err := module.EpochDir(epoch)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		if _, err := os.Stat(epochDir); os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "epoch %q is not installed.\n", epoch)
			os.Exit(1)
		}
		size := dirSize(epochDir)
		if err := os.RemoveAll(epochDir); err != nil {
			fmt.Fprintf(os.Stderr, "error removing %s: %v\n", epochDir, err)
			os.Exit(1)
		}
		fmt.Printf("Removed epoch %s (%s)\n", epoch, formatSize(size))
	}

	// Reclaim the removed epochs' EXCLUSIVE blobs while leaving blobs shared with
	// the remaining epochs intact (§4.4). The removed epoch dirs (and their
	// blobs.refs) are already gone, so the union of the remaining epochs' ref
	// sets is exactly the new live set.
	if store != nil {
		if liveBlobs, liveArchives, ok, lerr := blobstore.LiveSet(""); lerr == nil {
			m, _ := loadEmbeddedManifest()
			if res, serr := store.Sweep(liveBlobs, liveArchives, ok, m, false); serr == nil && res.BytesFreed > 0 {
				fmt.Printf("Reclaimed %s of unreferenced blobs.\n", formatSize(res.BytesFreed))
			}
		}
	}
}

// runFetch (a.k.a. warm) pre-stages the host workflow's dependency blobs into
// the CAS while online, so a thin binary can subsequently build offline (§4.4).
// It is the explicit, non-best-effort counterpart to the install-time prefetch
// (prefetchHostToolchain): on any failure — notably blobstore.OfflineError — it
// exits non-zero, since a user who ran `promise fetch` offline needs that
// signal. It reuses the exact materialization path a build takes, so the blobs
// land in the shared CAS just as the install prefetch does.
func runFetch(args []string) {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			fmt.Fprintf(os.Stderr, "unknown flag: %s\nusage: promise fetch\n", arg)
			os.Exit(1)
		}
	}
	// The user already opted in by running fetch — announce, don't re-prompt.
	prefetchNoPrompt = true
	defer func() { prefetchNoPrompt = false }()

	viewDir, err := resolveLLVMView(true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if runtime.GOOS == "linux" {
		target := "x86_64-unknown-linux-musl"
		if runtime.GOARCH == "arm64" {
			target = "aarch64-unknown-linux-musl"
		}
		if _, err := findMuslCRT(target); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	}

	if viewDir == "" {
		fmt.Println("This build resolves its toolchain from PATH — nothing to pre-stage.")
		return
	}
	blobCount := 0
	if store, serr := blobstore.NewStore(); serr == nil {
		if blobs, lerr := store.ListBlobs(); lerr == nil {
			blobCount = len(blobs)
		}
	}
	fmt.Printf("Host toolchain staged into the dependency cache (%d blobs).\n", blobCount)
	fmt.Printf("  view: %s\n", viewDir)
	fmt.Println("Offline builds with this epoch are now ready.")
}

// runGC bounds the dependency cache with union-rooted mark-and-sweep (§4.4):
// the live set is the union of ALL installed epochs' blobs.refs, so a blob
// shared by two epochs is never deleted while removing one. Holds the same
// exclusive lock as install/fetch.
func runGC(args []string) {
	dryRun := false
	jsonOut := false
	for _, arg := range args {
		switch arg {
		case "-dry-run", "--dry-run":
			dryRun = true
		case "-json", "--json":
			jsonOut = true
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\nusage: promise gc [-dry-run] [-json]\n", arg)
			os.Exit(1)
		}
	}

	store, err := blobstore.NewStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot open dependency cache: %v\n", err)
		os.Exit(1)
	}
	unlock, err := store.Lock("gc")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot lock dependency cache: %v\n", err)
		os.Exit(1)
	}
	defer unlock()

	liveBlobs, liveArchives, allRefsReadable, err := blobstore.LiveSet("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot compute live set: %v\n", err)
		os.Exit(1)
	}
	m, _ := loadEmbeddedManifest()
	res, err := store.Sweep(liveBlobs, liveArchives, allRefsReadable, m, dryRun)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: gc sweep failed: %v\n", err)
		os.Exit(1)
	}

	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(map[string]any{
			"dry_run":           dryRun,
			"all_refs_readable": allRefsReadable,
			"blobs_removed":     res.BlobsRemoved,
			"archives_removed":  res.ArchivesRemoved,
			"bytes_freed":       res.BytesFreed,
			"removed":           res.Removed,
		})
		return
	}

	if !allRefsReadable {
		fmt.Println("Kept all blobs: an installed epoch's blobs.refs is missing or unreadable (fail-safe).")
		return
	}
	verb := "Reclaimed"
	if dryRun {
		verb = "Would reclaim"
	}
	fmt.Printf("%s %s (%d blobs, %d archives).\n", verb, formatSize(res.BytesFreed), res.BlobsRemoved, res.ArchivesRemoved)
}

// dirSize computes the total size of all files under a directory.
func dirSize(path string) int64 {
	var total int64
	filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// formatSize formats a byte count as a human-readable string.
func formatSize(bytes int64) string {
	const mb = 1024 * 1024
	if bytes >= mb {
		return fmt.Sprintf("%d MB", bytes/mb)
	}
	const kb = 1024
	if bytes >= kb {
		return fmt.Sprintf("%d KB", bytes/kb)
	}
	return fmt.Sprintf("%d B", bytes)
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

// copyFile copies a single file from src to dst with the given permissions via a
// temp file in dst's directory followed by an atomic rename, so the destination
// always lands on a FRESH inode (T0722). This is the standard safe-replace: the
// installed binary can never inherit poisoned vnode/kernel state attached to a
// prior inode at dst (the macOS amfid-wedge failure), and overwriting a running
// executable cannot hit ETXTBSY or expose a partially written binary.
func copyFile(src, dst string, perm os.FileMode) {
	data, err := os.ReadFile(src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading %s: %v\n", src, err)
		os.Exit(1)
	}
	if err := writeFileAtomic(dst, data, perm); err != nil {
		fmt.Fprintf(os.Stderr, "error writing %s: %v\n", dst, err)
		os.Exit(1)
	}
}
