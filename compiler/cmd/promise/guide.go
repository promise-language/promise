package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/promise-language/promise/compiler/internal/module"
)

func runGuide(args []string) {
	var section string
	var showPath, install, showHelp bool

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-help":
			showHelp = true
		case "-path":
			showPath = true
		case "-install":
			install = true
		case "-section":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "error: -section requires a section name")
				fmt.Fprintln(os.Stderr, "usage: promise guide -section <name>")
				fmt.Fprintln(os.Stderr, "\nAvailable sections:")
				printSectionList()
				os.Exit(1)
			}
			i++
			section = args[i]
		default:
			fmt.Fprintf(os.Stderr, "unknown option: %s\n", args[i])
			printGuideUsage()
			os.Exit(1)
		}
	}

	if showHelp {
		printGuideUsage()
		return
	}

	if install {
		path, err := installGuide()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("installed: %s\n", path)
		return
	}

	if showPath {
		path, err := installGuide()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(path)
		return
	}

	if section != "" {
		text := extractSection(string(embeddedGuide), section)
		if text == "" {
			fmt.Fprintf(os.Stderr, "error: unknown section %q\n", section)
			fmt.Fprintln(os.Stderr, "\nAvailable sections:")
			printSectionList()
			os.Exit(1)
		}
		fmt.Print(text)
		return
	}

	// Default: print full guide.
	fmt.Print(string(embeddedGuide))
}

// installGuide extracts the guide to ~/.promise/docs/language-guide.md.
// Re-extracts if the compiler version has changed.
func installGuide() (string, error) {
	home, err := module.PromiseHome()
	if err != nil {
		return "", err
	}

	docsDir := filepath.Join(home, "docs")
	guidePath := filepath.Join(docsDir, "language-guide.md")
	stampPath := filepath.Join(docsDir, ".guide-version")

	// Check version stamp — skip extraction if current.
	v := guideVersion()
	if stamp, err := os.ReadFile(stampPath); err == nil && string(stamp) == v {
		return guidePath, nil
	}

	if err := os.MkdirAll(docsDir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(guidePath, embeddedGuide, 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(stampPath, []byte(v), 0o644); err != nil {
		return "", err
	}
	return guidePath, nil
}

// guideVersion returns the compiler version string for stamp comparison.
func guideVersion() string {
	if version != "" {
		return version
	}
	if epoch, err := module.CompilerEpoch(embeddedCatalog); err == nil {
		return epoch
	}
	return "unknown"
}

// extractSection returns the content of the named section (## heading).
// Matching is case-insensitive and allows hyphens for spaces
// (e.g., "error-handling" matches "## Error Handling").
func extractSection(guide, name string) string {
	slug := sectionSlug(name)
	guide = strings.ReplaceAll(guide, "\r\n", "\n")
	lines := strings.Split(guide, "\n")

	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "## ") {
			if sectionSlug(strings.TrimPrefix(line, "## ")) == slug {
				start = i
				continue
			}
			if start >= 0 {
				// Reached next section — return accumulated content.
				return strings.Join(lines[start:i], "\n") + "\n"
			}
		}
	}
	if start >= 0 {
		// Last section in the file.
		return strings.Join(lines[start:], "\n")
	}
	return ""
}

// sectionSlug normalizes a section name for matching:
// lowercase, spaces/underscores become hyphens, strip parens and special chars.
func sectionSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "-")
	s = strings.ReplaceAll(s, "_", "-")
	// Remove parenthetical suffixes like "(auto-imported from std)".
	if idx := strings.Index(s, "("); idx > 0 {
		s = strings.TrimRight(s[:idx], "-")
	}
	// Remove & characters.
	s = strings.ReplaceAll(s, "&", "")
	// Collapse consecutive hyphens (e.g., "ownership--borrowing" → "ownership-borrowing").
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	return s
}

// listSections returns the ## heading names from the guide.
func listSections() []string {
	var sections []string
	for _, line := range strings.Split(string(embeddedGuide), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "## ") {
			sections = append(sections, strings.TrimPrefix(line, "## "))
		}
	}
	return sections
}

func printSectionList() {
	for _, s := range listSections() {
		slug := sectionSlug(s)
		fmt.Fprintf(os.Stderr, "  %-30s  (-section %s)\n", s, slug)
	}
}

func printGuideUsage() {
	fmt.Print(`Usage: promise guide [options]

Print the Promise language reference guide.

Options:
  -section <name>    Print a single section (e.g., -section error-handling)
  -path              Print path to installed guide file
  -install           Extract guide to ~/.promise/docs/
  -help              Show this help

Examples:
  promise guide                          Full guide to stdout
  promise guide -section basics          Just the Basics section
  promise guide -section error-handling  Error handling reference
  promise guide -path                    Path for file-based access
`)
}
