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
// This is a no-op equivalent — always regenerates (like make generate).
func GenerateParser(root string) error {
	if err := DownloadAntlr(root); err != nil {
		return err
	}

	jar := AntlrJarPath(root)
	grammarDir := filepath.Join(root, "compiler", "grammar")
	parserPkg := filepath.Join(root, "compiler", "internal", "parser")

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
