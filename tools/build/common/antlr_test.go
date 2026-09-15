package common

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGrammarHashIgnoresLineEndings guards the Windows regression (T1407): a
// CRLF checkout (git core.autocrlf) must produce the same grammar hash as the
// LF working tree, otherwise the committed sidecar never matches and the parser
// regenerates on every build.
func TestGrammarHashIgnoresLineEndings(t *testing.T) {
	lfDir := t.TempDir()
	crlfDir := t.TempDir()

	lexer := "lexer grammar L;\nWS: [ \\t]+ -> skip;\n"
	parser := "parser grammar P;\nr: WS* EOF;\n"

	write := func(dir, name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	toCRLF := func(s string) string {
		out := make([]byte, 0, len(s)+8)
		for i := 0; i < len(s); i++ {
			if s[i] == '\n' {
				out = append(out, '\r')
			}
			out = append(out, s[i])
		}
		return string(out)
	}

	write(lfDir, "A.g4", lexer)
	write(lfDir, "B.g4", parser)
	write(crlfDir, "A.g4", toCRLF(lexer))
	write(crlfDir, "B.g4", toCRLF(parser))

	lfHash, err := grammarHash(lfDir)
	if err != nil {
		t.Fatal(err)
	}
	crlfHash, err := grammarHash(crlfDir)
	if err != nil {
		t.Fatal(err)
	}
	if lfHash != crlfHash {
		t.Fatalf("hash differs across line endings: LF=%s CRLF=%s", lfHash, crlfHash)
	}
}

// writeGeneratedParserTree lays out the two directories GenerateParser reads —
// compiler/grammar and compiler/internal/parser — under root, with one grammar
// file. It returns both paths.
func writeGeneratedParserTree(t *testing.T, root string) (grammarDir, parserPkg string) {
	t.Helper()
	grammarDir = filepath.Join(root, "compiler", "grammar")
	parserPkg = filepath.Join(root, "compiler", "internal", "parser")
	for _, dir := range []string{grammarDir, parserPkg} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(grammarDir, "A.g4"), []byte("grammar A;\nr: EOF;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return grammarDir, parserPkg
}

// TestGenerateParserRefusesWithoutGrammar is the regression test for the network
// half of T2116: over a root that is not a checkout, generation used to reach
// DownloadAntlr and fetch 2.1 MB from antlr.org — from inside bin/verify, which
// is mandatory before every commit and has no business making a request at all.
//
// The input has to be checked before the tool that consumes it: there is nothing
// to generate from, so the run must say so and stop. Asserted for both the plain
// and the --generate form, since forcing skips the up-to-date check and would
// otherwise walk straight into the fetch.
func TestGenerateParserRefusesWithoutGrammar(t *testing.T) {
	for _, force := range []bool{false, true} {
		name := "build"
		if force {
			name = "generate"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir() // no compiler/grammar at all
			err := GenerateParser(root, force)
			if err == nil {
				t.Fatal("expected an error when there is no grammar to generate from")
			}
			if !strings.Contains(err.Error(), "no .g4 files") {
				t.Errorf("error should name the missing grammar, got: %v", err)
			}
			if Exists(AntlrJarPath(root)) {
				t.Error("the ANTLR jar was fetched — no build path may reach the network (T2116)")
			}
		})
	}
}

// TestGenerateParserSkipsWhenSidecarMatches is the other half of the same
// property, and the one that holds on the real tree: the generated parser and
// its hash sidecar are committed, so a build finds them current and never enters
// generation — which is why bin/verify needs neither the network nor a JVM
// (T1407). Nothing here can reach either, so a regression shows up as a failure
// rather than as a slow run.
func TestGenerateParserSkipsWhenSidecarMatches(t *testing.T) {
	root := t.TempDir()
	grammarDir, parserPkg := writeGeneratedParserTree(t, root)
	if err := os.WriteFile(filepath.Join(parserPkg, "promise_parser.go"), []byte("package parser\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := grammarHash(grammarDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(grammarHashPath(parserPkg), []byte(h+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := GenerateParser(root, false); err != nil {
		t.Fatalf("a current sidecar must skip generation, got: %v", err)
	}
	if Exists(AntlrJarPath(root)) {
		t.Error("the ANTLR jar was fetched despite the parser being up to date")
	}
}

// TestParserUpToDate exercises the sidecar-driven staleness check end to end.
func TestParserUpToDate(t *testing.T) {
	dir := t.TempDir()
	grammarDir := filepath.Join(dir, "grammar")
	parserPkg := filepath.Join(dir, "parser")
	if err := os.MkdirAll(grammarDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(parserPkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(grammarDir, "A.g4"), []byte("grammar A;\nr: EOF;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// No generated parser yet → not up to date.
	if parserUpToDate(grammarDir, parserPkg) {
		t.Fatal("expected not up to date with no generated parser")
	}
	if err := os.WriteFile(filepath.Join(parserPkg, "promise_parser.go"), []byte("package parser\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Parser present but no sidecar → not up to date.
	if parserUpToDate(grammarDir, parserPkg) {
		t.Fatal("expected not up to date with missing sidecar")
	}
	// Write the matching sidecar → up to date.
	h, err := grammarHash(grammarDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(grammarHashPath(parserPkg), []byte(h+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !parserUpToDate(grammarDir, parserPkg) {
		t.Fatal("expected up to date with matching sidecar")
	}
	// Change the grammar → stale again.
	if err := os.WriteFile(filepath.Join(grammarDir, "A.g4"), []byte("grammar A;\nr: WS EOF;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if parserUpToDate(grammarDir, parserPkg) {
		t.Fatal("expected stale after grammar change")
	}
}
