package common

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

	// Record the grammar hash so a later build can tell whether the committed
	// parser is up to date without relying on file mtimes (which git does not
	// preserve across checkouts).
	hash, err := grammarHash(grammarDir)
	if err != nil {
		return fmt.Errorf("hash grammar: %w", err)
	}
	if err := os.WriteFile(grammarHashPath(parserPkg), []byte(hash+"\n"), 0o644); err != nil {
		return fmt.Errorf("write grammar hash: %w", err)
	}

	return nil
}

// grammarHashPath is the sidecar file recording the hash of the .g4 grammar
// that produced the current generated parser. It is committed alongside the
// generated Go parser files.
func grammarHashPath(parserPkg string) string {
	return filepath.Join(parserPkg, ".grammar.hash")
}

// grammarHash computes an FNV-128a hash of all .g4 grammar files (name + size +
// contents), independent of file modification times. Line endings are
// normalized to LF before hashing so the result is identical whether git checks
// the grammar out with LF (POSIX) or CRLF (Windows core.autocrlf) — otherwise a
// CRLF checkout would never match the LF-committed sidecar and would regenerate
// on every build. Matches the content-hash pattern used by ToolsSourceHash.
func grammarHash(grammarDir string) (string, error) {
	g4s, err := filepath.Glob(filepath.Join(grammarDir, "*.g4"))
	if err != nil {
		return "", err
	}
	if len(g4s) == 0 {
		return "", fmt.Errorf("no .g4 files in %s", grammarDir)
	}
	sort.Strings(g4s)

	h := fnv.New128a()
	for _, g4 := range g4s {
		data, err := os.ReadFile(g4)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", g4, err)
		}
		data = normalizeLineEndings(data)
		fmt.Fprintf(h, "%s\n%d\n", filepath.Base(g4), len(data))
		h.Write(data)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// normalizeLineEndings collapses CRLF and lone CR to LF so content hashes are
// stable across platforms regardless of git's checkout line-ending policy.
func normalizeLineEndings(data []byte) []byte {
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	return bytes.ReplaceAll(data, []byte("\r"), []byte("\n"))
}

// parserUpToDate returns true if the generated parser exists and the committed
// grammar-hash sidecar matches the current .g4 grammar. This is deterministic
// across fresh git checkouts, where file mtimes carry no ordering information
// (T1407 — the old mtime comparison spuriously regenerated on Windows clones).
func parserUpToDate(grammarDir, parserPkg string) bool {
	// The key output file must exist.
	if !Exists(filepath.Join(parserPkg, "promise_parser.go")) {
		return false
	}

	want, err := grammarHash(grammarDir)
	if err != nil {
		return false
	}
	got, err := os.ReadFile(grammarHashPath(parserPkg))
	if err != nil {
		return false // no sidecar → regenerate (and create it)
	}
	return strings.TrimSpace(string(got)) == want
}
