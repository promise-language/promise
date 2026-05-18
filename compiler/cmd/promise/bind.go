package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"djabi.dev/go/promise_lang/internal/bindgen"
	"djabi.dev/go/promise_lang/internal/webidl"
	"djabi.dev/go/promise_lang/internal/wit"
)

func runBind(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: promise bind <format> [options] <files...>")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "formats:")
		fmt.Fprintln(os.Stderr, "  wit       Generate bindings from WIT definitions")
		fmt.Fprintln(os.Stderr, "  webidl    Generate bindings from WebIDL definitions")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "examples:")
		fmt.Fprintln(os.Stderr, "  promise bind wit path/to/api.wit -o modules/wasi/")
		fmt.Fprintln(os.Stderr, "  promise bind webidl path/to/dom.webidl -o modules/web/")
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

func runBindWit(args []string) {
	var (
		outDir       = "."
		moduleName   = ""
		target       = "wasi"
		canonicalABI = false
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
	tomlContent := fmt.Sprintf("[module]\nname = \"%s\"\nepoch = \"2026.0\"\n", moduleName)
	if err := os.WriteFile(tomlPath, []byte(tomlContent), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "error writing %s: %v\n", tomlPath, err)
		os.Exit(1)
	}
	fmt.Println(tomlPath)
}

func runBindWebIdl(args []string) {
	var (
		outDir     = "."
		moduleName = ""
		target     = "web"
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
	tomlContent := fmt.Sprintf("[module]\nname = \"%s\"\nepoch = \"2026.0\"\n", moduleName)
	if err := os.WriteFile(tomlPath, []byte(tomlContent), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "error writing %s: %v\n", tomlPath, err)
		os.Exit(1)
	}
	fmt.Println(tomlPath)
}
