package common

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

const (
	antlrVersion = "4.13.1"
	antlrURL     = "https://www.antlr.org/download/antlr-" + antlrVersion + "-complete.jar"
)

// AntlrJarPath returns the path to the ANTLR JAR file.
func AntlrJarPath(root string) string {
	return filepath.Join(root, "compiler", "tools", "antlr-"+antlrVersion+"-complete.jar")
}

// DownloadAntlr downloads the ANTLR JAR if it doesn't exist.
func DownloadAntlr(root string) error {
	jar := AntlrJarPath(root)
	if Exists(jar) {
		return nil
	}
	dir := filepath.Dir(jar)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	fmt.Printf("Downloading ANTLR %s...\n", antlrVersion)

	resp, err := http.Get(antlrURL)
	if err != nil {
		return fmt.Errorf("download ANTLR: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download ANTLR: HTTP %d", resp.StatusCode)
	}

	out, err := os.Create(jar)
	if err != nil {
		return fmt.Errorf("create %s: %w", jar, err)
	}
	defer out.Close()

	if _, err := io.Copy(out, resp.Body); err != nil {
		os.Remove(jar) // clean up partial download
		return fmt.Errorf("download ANTLR: %w", err)
	}
	return out.Close()
}

// GenerateParser runs ANTLR to generate the Go lexer and parser from the grammar files.
// If force is false, skips generation when parser output already exists and is up to date.
func GenerateParser(root string, force bool) error {
	grammarDir := filepath.Join(root, "compiler", "grammar")
	parserPkg := filepath.Join(root, "compiler", "internal", "parser")

	if !force && parserUpToDate(grammarDir, parserPkg) {
		fmt.Println("  Parser up to date (use --generate to force)")
		return nil
	}

	if err := DownloadAntlr(root); err != nil {
		return err
	}

	jar := AntlrJarPath(root)

	if err := os.MkdirAll(parserPkg, 0o755); err != nil {
		return fmt.Errorf("mkdir parser: %w", err)
	}

	// Lexer
	if err := RunIn(grammarDir, "java", "-jar", jar,
		"-Dlanguage=Go",
		"-package", "parser",
		"-visitor",
		"-o", parserPkg,
		"PromiseLexer.g4",
	); err != nil {
		return fmt.Errorf("generate lexer: %w", err)
	}

	// Parser
	if err := RunIn(grammarDir, "java", "-jar", jar,
		"-Dlanguage=Go",
		"-package", "parser",
		"-visitor",
		"-lib", parserPkg,
		"-o", parserPkg,
		"PromiseParser.g4",
	); err != nil {
		return fmt.Errorf("generate parser: %w", err)
	}

	return nil
}

// parserUpToDate returns true if the generated parser files exist and are
// newer than the grammar source files (.g4).
func parserUpToDate(grammarDir, parserPkg string) bool {
	// Check that the key output file exists.
	parserFile := filepath.Join(parserPkg, "promise_parser.go")
	parserInfo, err := os.Stat(parserFile)
	if err != nil {
		return false
	}

	// Find the newest grammar file.
	g4s, err := filepath.Glob(filepath.Join(grammarDir, "*.g4"))
	if err != nil || len(g4s) == 0 {
		return false
	}
	for _, g4 := range g4s {
		info, err := os.Stat(g4)
		if err != nil {
			return false
		}
		if info.ModTime().After(parserInfo.ModTime()) {
			return false // grammar is newer than parser output
		}
	}
	return true
}
