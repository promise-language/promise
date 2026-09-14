package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/promise-language/promise/compiler/internal/bindgen"
	"github.com/promise-language/promise/compiler/internal/module"
	"github.com/promise-language/promise/compiler/internal/webidl"
	"github.com/promise-language/promise/compiler/internal/wit"
)

// bindEpoch returns the running compiler's epoch for generated promise.toml
// scaffolds, falling back to a recent epoch if the catalog can't be read (T0972).
func bindEpoch() string {
	epoch, err := module.CompilerEpoch(embeddedCatalog)
	if err != nil || epoch == "" {
		return "2026.1"
	}
	return epoch
}

// runBindSelfCheck type-checks the project bind just wrote to outDir, for the
// target it generated for, and exits non-zero if it doesn't resolve
// (docs/wasm-web-callbacks.md §16). Frontend only — no subprocess, no LLVM,
// no linker: parse → merge std → sema → embeds → ownership, the same pipeline
// `promise build` runs, reusing the exact discovery (discoverProject) and
// frontend (compileProjectFrontend) entry points build already uses, so a
// project that fails here would have failed identically one command later at
// `promise build` — the #25 review's "undefined type: EventListener" gap this
// closes. compileProjectFrontend itself os.Exit(1)s with the sema/ownership
// errors printed, so success here just means "returned" — the target passed
// in is bind's own `-target` (default web / wasi — see runBindWebIdl/
// runBindWit), always web/wasi-flavored, so the triple is always "wasm32-<target>".
func runBindSelfCheck(outDir, target string) {
	cfg, files, err := discoverProject(outDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: bind self-check: %v\n", err)
		os.Exit(1)
	}
	dir := outDir
	if cfg != nil {
		dir = cfg.Dir
	}
	compileProjectFrontend(dir, files, "wasm32-"+target)
}

func runBind(args []string) {
	if len(args) == 0 {
		printBindUsage(os.Stderr)
		os.Exit(1)
	}

	switch args[0] {
	case "wit":
		runBindWit(args[1:])
	case "webidl":
		runBindWebIdl(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown bind format: %s\n", args[0])
		fmt.Fprintln(os.Stderr, "supported formats: wit, webidl")
		os.Exit(1)
	}
}

// printBindUsage writes `promise bind` usage to w. Shared between the no-format
// usage error and the central help tree (T1006).
func printBindUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: promise bind <format> [options] <files...>")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "formats:")
	fmt.Fprintln(w, "  wit       Generate bindings from WIT definitions")
	fmt.Fprintln(w, "  webidl    Generate bindings from WebIDL definitions")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "examples:")
	fmt.Fprintln(w, "  promise bind wit path/to/api.wit -o modules/wasi/")
	fmt.Fprintln(w, "  promise bind webidl path/to/dom.webidl -o modules/web/")
}

func runBindWit(args []string) {
	var (
		outDir       = "."
		moduleName   = ""
		target       = "wasi"
		canonicalABI = false
		noCheck      = false
		files        []string
	)

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-o":
			i++
			if i >= len(args) {
				fmt.Fprintln(os.Stderr, "-o requires an argument")
				os.Exit(1)
			}
			outDir = args[i]
		case "-name":
			i++
			if i >= len(args) {
				fmt.Fprintln(os.Stderr, "-name requires an argument")
				os.Exit(1)
			}
			moduleName = args[i]
		case "-target":
			i++
			if i >= len(args) {
				fmt.Fprintln(os.Stderr, "-target requires an argument")
				os.Exit(1)
			}
			target = args[i]
		case "-canonical-abi":
			canonicalABI = true
		case "-no-check":
			noCheck = true
		default:
			files = append(files, args[i])
		}
	}

	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "usage: promise bind wit [options] <files...>")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "options:")
		fmt.Fprintln(os.Stderr, "  -o <dir>        output directory (default: .)")
		fmt.Fprintln(os.Stderr, "  -name <name>    module name (default: derived from WIT package)")
		fmt.Fprintln(os.Stderr, "  -target <t>     target annotation: wasi, web (default: wasi)")
		fmt.Fprintln(os.Stderr, "  -no-check       skip type-checking the generated output (§16)")
		os.Exit(1)
	}

	// Expand directories
	var witFiles []string
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		if info.IsDir() {
			entries, err := os.ReadDir(f)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error reading directory: %v\n", err)
				os.Exit(1)
			}
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".wit") {
					witFiles = append(witFiles, filepath.Join(f, e.Name()))
				}
			}
		} else {
			witFiles = append(witFiles, f)
		}
	}

	if len(witFiles) == 0 {
		fmt.Fprintln(os.Stderr, "no .wit files found")
		os.Exit(1)
	}

	// Parse all WIT files
	var allModules []*bindgen.Module
	var pkgName string

	for _, path := range witFiles {
		src, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error reading %s: %v\n", path, err)
			os.Exit(1)
		}

		file, errs := wit.Parse(string(src), path)
		if len(errs) > 0 {
			for _, e := range errs {
				fmt.Fprintln(os.Stderr, e.Error())
			}
			os.Exit(1)
		}

		// Derive module name from first package declaration
		if pkgName == "" && file.Package != nil {
			pkgName = file.Package.Name
		}

		modules := bindgen.WitToIR(file)
		allModules = append(allModules, modules...)
	}

	// Determine final module name
	if moduleName == "" {
		if pkgName != "" {
			moduleName = strings.ReplaceAll(pkgName, "-", "_")
		} else {
			// Derive from first input file
			base := filepath.Base(witFiles[0])
			moduleName = strings.TrimSuffix(base, ".wit")
			moduleName = strings.ReplaceAll(moduleName, "-", "_")
		}
	}

	// Generate Promise source
	prSource := bindgen.GeneratePromiseWithOptions(allModules, target, canonicalABI)

	// Create output directory
	if err := os.MkdirAll(outDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "error creating output directory: %v\n", err)
		os.Exit(1)
	}

	// Write .pr file
	prPath := filepath.Join(outDir, moduleName+".pr")
	if err := os.WriteFile(prPath, []byte(prSource), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "error writing %s: %v\n", prPath, err)
		os.Exit(1)
	}
	fmt.Println(prPath)

	// Write promise.toml
	tomlPath := filepath.Join(outDir, "promise.toml")
	tomlContent := fmt.Sprintf("[module]\nname = %q\nepoch = %q\n", moduleName, bindEpoch())
	if err := os.WriteFile(tomlPath, []byte(tomlContent), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "error writing %s: %v\n", tomlPath, err)
		os.Exit(1)
	}
	fmt.Println(tomlPath)

	if !noCheck {
		runBindSelfCheck(outDir, target)
	}
}

func runBindWebIdl(args []string) {
	var (
		outDir     = "."
		moduleName = ""
		target     = "web"
		noCheck    = false
		files      []string
	)

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-o":
			i++
			if i >= len(args) {
				fmt.Fprintln(os.Stderr, "-o requires an argument")
				os.Exit(1)
			}
			outDir = args[i]
		case "-name":
			i++
			if i >= len(args) {
				fmt.Fprintln(os.Stderr, "-name requires an argument")
				os.Exit(1)
			}
			moduleName = args[i]
		case "-target":
			i++
			if i >= len(args) {
				fmt.Fprintln(os.Stderr, "-target requires an argument")
				os.Exit(1)
			}
			target = args[i]
		case "-no-check":
			noCheck = true
		default:
			files = append(files, args[i])
		}
	}

	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "usage: promise bind webidl [options] <files...>")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "options:")
		fmt.Fprintln(os.Stderr, "  -o <dir>        output directory (default: .)")
		fmt.Fprintln(os.Stderr, "  -name <name>    module name (default: derived from first interface)")
		fmt.Fprintln(os.Stderr, "  -target <t>     target annotation: web, wasi (default: web)")
		fmt.Fprintln(os.Stderr, "  -no-check       skip type-checking the generated output (§16)")
		os.Exit(1)
	}

	// Expand directories
	var idlFiles []string
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		if info.IsDir() {
			entries, err := os.ReadDir(f)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error reading directory: %v\n", err)
				os.Exit(1)
			}
			for _, e := range entries {
				if !e.IsDir() && (strings.HasSuffix(e.Name(), ".webidl") || strings.HasSuffix(e.Name(), ".idl")) {
					idlFiles = append(idlFiles, filepath.Join(f, e.Name()))
				}
			}
		} else {
			idlFiles = append(idlFiles, f)
		}
	}

	if len(idlFiles) == 0 {
		fmt.Fprintln(os.Stderr, "no .webidl or .idl files found")
		os.Exit(1)
	}

	// Parse all WebIDL files into a single merged file
	mergedFile := &webidl.File{}

	for _, path := range idlFiles {
		src, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error reading %s: %v\n", path, err)
			os.Exit(1)
		}

		file, errs := webidl.Parse(string(src), path)
		if len(errs) > 0 {
			for _, e := range errs {
				fmt.Fprintln(os.Stderr, e.Error())
			}
			os.Exit(1)
		}

		mergedFile.Interfaces = append(mergedFile.Interfaces, file.Interfaces...)
		mergedFile.Dictionaries = append(mergedFile.Dictionaries, file.Dictionaries...)
		mergedFile.Enums = append(mergedFile.Enums, file.Enums...)
		mergedFile.Callbacks = append(mergedFile.Callbacks, file.Callbacks...)
		mergedFile.Typedefs = append(mergedFile.Typedefs, file.Typedefs...)
		mergedFile.Partials = append(mergedFile.Partials, file.Partials...)
		mergedFile.Mixins = append(mergedFile.Mixins, file.Mixins...)
		mergedFile.Includes = append(mergedFile.Includes, file.Includes...)
	}

	// Merge partials, mixins, and includes
	webidl.Merge(mergedFile)

	// Convert to binding IR
	allModules := bindgen.WebIdlToIR(mergedFile)

	// Determine final module name — use the IR module name (derived via idlToSnake)
	// to stay consistent with what GeneratePromise emits.
	if moduleName == "" {
		if len(allModules) > 0 && allModules[0].Name != "web" {
			moduleName = allModules[0].Name
		} else {
			base := filepath.Base(idlFiles[0])
			moduleName = strings.TrimSuffix(strings.TrimSuffix(base, ".webidl"), ".idl")
			moduleName = strings.ReplaceAll(moduleName, "-", "_")
		}
	}

	// Generate Promise source
	prSource := bindgen.GeneratePromise(allModules, target)

	// Generate JS glue
	jsSource := bindgen.GenerateJSGlue(allModules)

	// Create output directory
	if err := os.MkdirAll(outDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "error creating output directory: %v\n", err)
		os.Exit(1)
	}

	// Write .pr file
	prPath := filepath.Join(outDir, moduleName+".pr")
	if err := os.WriteFile(prPath, []byte(prSource), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "error writing %s: %v\n", prPath, err)
		os.Exit(1)
	}
	fmt.Println(prPath)

	// Write .js file
	jsPath := filepath.Join(outDir, moduleName+".js")
	if err := os.WriteFile(jsPath, []byte(jsSource), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "error writing %s: %v\n", jsPath, err)
		os.Exit(1)
	}
	fmt.Println(jsPath)

	// Write promise.toml
	tomlPath := filepath.Join(outDir, "promise.toml")
	tomlContent := fmt.Sprintf("[module]\nname = %q\nepoch = %q\n", moduleName, bindEpoch())
	if err := os.WriteFile(tomlPath, []byte(tomlContent), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "error writing %s: %v\n", tomlPath, err)
		os.Exit(1)
	}
	fmt.Println(tomlPath)

	if !noCheck {
		runBindSelfCheck(outDir, target)
	}
}
