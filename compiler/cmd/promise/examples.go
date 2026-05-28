package main

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/promise-language/promise/compiler/internal/module"
)

// exampleEntry represents a single example file.
type exampleEntry struct {
	category string // e.g., "01_basics"
	name     string // e.g., "hello" (stem without .pr)
	path     string // relative path in embedded FS, e.g., "resources/examples/01_basics/hello.pr"
	desc     string // first-line comment stripped of "// " prefix
}

func runExamples(args []string) {
	var showDir, showRun, showHelp bool
	var name string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-help":
			showHelp = true
		case "-dir":
			showDir = true
		case "-run":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "error: -run requires an example name")
				fmt.Fprintln(os.Stderr, "usage: promise examples -run <name>")
				os.Exit(1)
			}
			i++
			name = args[i]
			showRun = true
		default:
			if strings.HasPrefix(args[i], "-") {
				fmt.Fprintf(os.Stderr, "unknown option: %s\n", args[i])
				printExamplesUsage()
				os.Exit(1)
			}
			name = args[i]
		}
	}

	if showHelp {
		printExamplesUsage()
		return
	}

	if showDir {
		path, err := installExamples()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(path)
		return
	}

	if showRun {
		path, err := installExamples()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		filePath := findExampleFile(path, name)
		if filePath == "" {
			fmt.Fprintf(os.Stderr, "error: unknown example %q\n", name)
			fmt.Fprintln(os.Stderr, "\nAvailable examples:")
			listExampleNames(os.Stderr)
			os.Exit(1)
		}
		runRun([]string{filePath})
		return
	}

	if name != "" {
		entry := findEmbeddedExample(name)
		if entry == nil {
			fmt.Fprintf(os.Stderr, "error: unknown example %q\n", name)
			fmt.Fprintln(os.Stderr, "\nAvailable examples:")
			listExampleNames(os.Stderr)
			os.Exit(1)
		}
		data, err := embeddedExamples.ReadFile(entry.path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error reading example: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(string(data))
		return
	}

	// Default: list all examples grouped by category.
	listExamples()
}

// loadExamples reads all example entries from the embedded FS.
func loadExamples() []exampleEntry {
	var entries []exampleEntry

	categories, err := embeddedExamples.ReadDir("resources/examples")
	if err != nil {
		return nil
	}

	for _, cat := range categories {
		if !cat.IsDir() {
			continue
		}
		files, err := embeddedExamples.ReadDir("resources/examples/" + cat.Name())
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".pr") {
				continue
			}
			relPath := "resources/examples/" + cat.Name() + "/" + f.Name()
			stem := strings.TrimSuffix(f.Name(), ".pr")
			desc := readFirstComment(relPath)
			entries = append(entries, exampleEntry{
				category: cat.Name(),
				name:     stem,
				path:     relPath,
				desc:     desc,
			})
		}
	}
	return entries
}

// readFirstComment reads the first line of a file and returns the comment text
// (without the "// " prefix). Returns "" if the first line is not a comment.
func readFirstComment(path string) string {
	data, err := embeddedExamples.ReadFile(path)
	if err != nil {
		return ""
	}
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	if sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "// ") {
			return strings.TrimPrefix(line, "// ")
		}
	}
	return ""
}

// listExamples prints all examples grouped by category.
func listExamples() {
	entries := loadExamples()
	if len(entries) == 0 {
		fmt.Fprintln(os.Stderr, "no examples found")
		return
	}

	// Group by category.
	type catGroup struct {
		name    string
		display string
		entries []exampleEntry
	}
	groupMap := map[string]*catGroup{}
	var order []string
	for _, e := range entries {
		g, ok := groupMap[e.category]
		if !ok {
			display := categoryDisplayName(e.category)
			g = &catGroup{name: e.category, display: display}
			groupMap[e.category] = g
			order = append(order, e.category)
		}
		g.entries = append(g.entries, e)
	}
	sort.Strings(order)

	for i, catName := range order {
		g := groupMap[catName]
		if i > 0 {
			fmt.Println()
		}
		fmt.Printf("%s\n", g.display)
		for _, e := range g.entries {
			if e.desc != "" {
				fmt.Printf("  %-25s %s\n", e.name, e.desc)
			} else {
				fmt.Printf("  %s\n", e.name)
			}
		}
	}

	fmt.Printf("\nUse 'promise examples <name>' to view source, 'promise examples -run <name>' to run.\n")
}

// categoryDisplayName converts "01_basics" to "Basics", "07_concurrency" to "Concurrency", etc.
func categoryDisplayName(dir string) string {
	// Strip numeric prefix like "01_".
	name := dir
	if idx := strings.Index(dir, "_"); idx >= 0 && idx < 3 {
		name = dir[idx+1:]
	}
	// Title case, replacing underscores with spaces.
	name = strings.ReplaceAll(name, "_", " ")
	words := strings.Fields(name)
	for i, w := range words {
		if len(w) > 0 {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

// exampleSlug normalizes an example name for matching.
func exampleSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSuffix(s, ".pr")
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, "-", "_")
	return s
}

// findEmbeddedExample finds an example by name (file stem or category/stem).
func findEmbeddedExample(name string) *exampleEntry {
	entries := loadExamples()
	slug := exampleSlug(name)

	// Try exact match on stem.
	for i := range entries {
		if exampleSlug(entries[i].name) == slug {
			return &entries[i]
		}
	}

	// Try category/stem match (e.g., "basics/hello" or "01_basics/hello").
	if strings.Contains(slug, "/") {
		parts := strings.SplitN(slug, "/", 2)
		catSlug, nameSlug := parts[0], parts[1]
		for i := range entries {
			if (exampleSlug(entries[i].category) == catSlug ||
				exampleSlug(categoryDisplayName(entries[i].category)) == catSlug) &&
				exampleSlug(entries[i].name) == nameSlug {
				return &entries[i]
			}
		}
	}

	// Try category match — if the name matches a category, return nil (ambiguous).
	// This case is handled at the caller level.
	return nil
}

// findExampleFile locates an example .pr file on disk after extraction.
func findExampleFile(examplesDir, name string) string {
	slug := exampleSlug(name)

	// Walk extracted examples directory.
	var match string
	filepath.WalkDir(examplesDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".pr") {
			return nil
		}
		stem := strings.TrimSuffix(d.Name(), ".pr")
		if exampleSlug(stem) == slug {
			match = path
			return filepath.SkipAll
		}
		return nil
	})

	if match != "" {
		return match
	}

	// Try category/stem pattern.
	if strings.Contains(slug, "/") {
		parts := strings.SplitN(slug, "/", 2)
		catSlug, nameSlug := parts[0], parts[1]
		filepath.WalkDir(examplesDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".pr") {
				return nil
			}
			stem := strings.TrimSuffix(d.Name(), ".pr")
			dir := filepath.Base(filepath.Dir(path))
			if (exampleSlug(dir) == catSlug || exampleSlug(categoryDisplayName(dir)) == catSlug) &&
				exampleSlug(stem) == nameSlug {
				match = path
				return filepath.SkipAll
			}
			return nil
		})
	}

	return match
}

// installExamples extracts embedded examples to ~/.promise/examples/.
// Re-extracts if the compiler version has changed.
func installExamples() (string, error) {
	home, err := module.PromiseHome()
	if err != nil {
		return "", err
	}

	exDir := filepath.Join(home, "examples")
	stampPath := filepath.Join(exDir, ".examples-version")

	// Check version stamp — skip extraction if current.
	v := examplesVersion()
	if stamp, err := os.ReadFile(stampPath); err == nil && string(stamp) == v {
		return exDir, nil
	}

	// Extract all examples.
	if err := os.MkdirAll(exDir, 0o755); err != nil {
		return "", err
	}

	err = fs.WalkDir(embeddedExamples, "resources/examples", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Relative path under resources/examples/.
		rel, err := filepath.Rel("resources/examples", path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		dest := filepath.Join(exDir, rel)
		if d.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}

		data, err := embeddedExamples.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0o644)
	})
	if err != nil {
		return "", err
	}

	// Write version stamp.
	if err := os.WriteFile(stampPath, []byte(v), 0o644); err != nil {
		return "", err
	}

	return exDir, nil
}

// examplesVersion returns the compiler version string for stamp comparison.
func examplesVersion() string {
	if version != "" {
		return version
	}
	if epoch, err := module.CompilerEpoch(embeddedCatalog); err == nil {
		return epoch
	}
	return "unknown"
}

// listExampleNames prints all example names to the given writer.
func listExampleNames(w *os.File) {
	entries := loadExamples()
	for _, e := range entries {
		fmt.Fprintf(w, "  %s\n", e.name)
	}
}

func printExamplesUsage() {
	fmt.Print(`Usage: promise examples [options] [name]

Browse and run example programs.

Commands:
  promise examples              List all examples with descriptions
  promise examples <name>       Print the source of an example
  promise examples -run <name>  Run an example directly
  promise examples -dir         Print path to extracted examples directory

Options:
  -run <name>    Run an example program
  -dir           Print extracted examples directory path
  -help          Show this help

Examples:
  promise examples                       List all available examples
  promise examples hello                 View the hello world example
  promise examples -run hello            Run the hello world example
  promise examples -run channels         Run the channels concurrency example
  promise examples -dir                  Get path for file-based access
`)
}
