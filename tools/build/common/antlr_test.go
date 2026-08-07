package common

import (
	"os"
	"path/filepath"
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
