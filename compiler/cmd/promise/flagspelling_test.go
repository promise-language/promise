package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// flagCaseScan is the result of scanning one file for switch arms that match
// flag spellings: `dead` holds a diagnostic per unreachable `--flag` arm, and
// `flagCases` counts the `case` clauses that mentioned any flag-shaped literal
// at all — the scan's own liveness signal, so a guard that stops seeing flag
// switches fails loudly instead of passing vacuously.
type flagCaseScan struct {
	dead      []string
	flagCases int
}

// scanFlagCases finds `case` arms that can never match. `main()` runs every
// argument through normalizeArgs, which rewrites `--flag` to `-flag` before
// dispatch, so a `case "--flag":` with no `"-flag"` twin is dead code. String
// literals are collected from anywhere inside the clause's expressions, so both
// `case "--x":` and `case a == "--x" || a == "-x":` are seen.
func scanFlagCases(fset *token.FileSet, f *ast.File) flagCaseScan {
	var scan flagCaseScan
	ast.Inspect(f, func(n ast.Node) bool {
		cc, ok := n.(*ast.CaseClause)
		if !ok {
			return true
		}
		lits := map[string]bool{}
		for _, expr := range cc.List {
			ast.Inspect(expr, func(m ast.Node) bool {
				if bl, ok := m.(*ast.BasicLit); ok && bl.Kind == token.STRING {
					if v, err := strconv.Unquote(bl.Value); err == nil {
						lits[v] = true
					}
				}
				return true
			})
		}
		sawFlag := false
		for lit := range lits {
			// `--` alone is the program-args separator (T1426), not a flag; a
			// lone `-` is stdin/stdout, likewise not a flag.
			if !strings.HasPrefix(lit, "-") || lit == "-" || lit == "--" {
				continue
			}
			sawFlag = true
			if !strings.HasPrefix(lit, "--") {
				continue
			}
			if !lits[lit[1:]] {
				scan.dead = append(scan.dead, fset.Position(cc.Pos()).String()+
					": case matches "+strconv.Quote(lit)+" but not the normalized "+strconv.Quote(lit[1:])+
					" — normalizeArgs rewrites --flag to -flag, so this arm is unreachable")
			}
		}
		if sawFlag {
			scan.flagCases++
		}
		return true
	})
	return scan
}

// TestFlagSpellingCasesAcceptBothForms guards the whole `promise` CLI against a
// dead flag arm: `main()` runs every argument through normalizeArgs, which
// rewrites `--flag` to `-flag` before dispatch, so a `case "--flag":` with no
// `"-flag"` twin can never match. The failure is invisible at runtime — in the
// benign case the flag is silently ignored, and in the case that motivated this
// guard (`promise update --force`, T1513) the flag fell through to the
// positional switch and made the command exit 1 while its own usage text kept
// documenting it. The same class was fixed once before in parseAddFlags (T1779).
//
// The scan is syntax-only, so build-tagged files (Windows/darwin-only) are
// checked on every host. Known limit: it covers `case` clauses, not `else if`
// chains — `runTest`'s arg loop is the package's one such chain and lists both
// spellings by hand, so it is correct but not machine-checked. Routing that
// loop through parseCLIArgs, the last command T1604 left hand-rolled, would
// bring it under this guard too (T2009).
// Scoped to this package deliberately: tools/build/cmd/guard switches on *git's*
// own `--force`/`--hard` flags, where the double-dash spelling is correct.
func TestFlagSpellingCasesAcceptBothForms(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	files, flagCases := 0, 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files++
		scan := scanFlagCases(fset, f)
		flagCases += scan.flagCases
		for _, d := range scan.dead {
			t.Error(d)
		}
	}
	// Non-vacuity: the guard is only meaningful if it actually read this
	// package's sources and found flag switches in them. Without these, a
	// mis-scoped directory or a broken literal walk would report "no dead arms"
	// on an empty scan and the guard would silently stop guarding.
	if files < 10 {
		t.Errorf("scanned only %d non-test .go files — the guard is not reading package main", files)
	}
	if flagCases == 0 {
		t.Error("no flag-shaped case literals found — the scan is not seeing the CLI's flag switches")
	}
}

// TestScanFlagCasesDetects exercises the detector itself over synthetic
// sources. A static guard whose detection logic is wrong reports a clean
// package forever, so the guard above is only worth its line count if the
// scanner is known to flag the shape it exists to catch — and, just as
// important, to leave the legal shapes alone (both spellings listed, the bare
// `--` separator, a lone `-`, non-flag strings).
func TestScanFlagCasesDetects(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantDead  int
		wantFlags int
	}{
		{"double dash only", `switch a { case "--force": _ = a }`, 1, 1},
		{"both spellings listed", `switch a { case "--force", "-force": _ = a }`, 0, 1},
		{"single dash only", `switch a { case "-force": _ = a }`, 0, 1},
		{"two aliases both covered", `switch a { case "--force", "-force", "--reinstall", "-reinstall": _ = a }`, 0, 1},
		{"two dead aliases in one arm", `switch a { case "--force", "--reinstall": _ = a }`, 2, 1},
		{"one of two aliases dead", `switch a { case "--force", "-force", "--reinstall": _ = a }`, 1, 1},
		{"bare separator is not a flag", `switch a { case "--": _ = a }`, 0, 0},
		{"lone dash is not a flag", `switch a { case "-": _ = a }`, 0, 0},
		{"non-flag strings", `switch a { case "check", "channel": _ = a }`, 0, 0},
		{"comparison both spellings", `switch { case a == "--force" || a == "-force": _ = a }`, 0, 1},
		{"comparison double only", `switch { case a == "--force": _ = a }`, 1, 1},
		{"separate arms do not cover each other", `switch a { case "--force": _ = a; case "-force": _ = a }`, 1, 2},
		{"nested inside a func literal", `f := func(b string) { switch b { case "--json": _ = b } }; _ = f`, 1, 1},
		{"type switch arms ignored", `var v any = a; switch v.(type) { case string: }`, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "package p\nfunc f(a string) {\n" + tc.body + "\n}\n"
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "synthetic.go", src, 0)
			if err != nil {
				t.Fatalf("parse synthetic source: %v\n%s", err, src)
			}
			scan := scanFlagCases(fset, f)
			if len(scan.dead) != tc.wantDead {
				t.Errorf("dead arms = %d, want %d: %v", len(scan.dead), tc.wantDead, scan.dead)
			}
			if scan.flagCases != tc.wantFlags {
				t.Errorf("flag cases = %d, want %d", scan.flagCases, tc.wantFlags)
			}
			for _, d := range scan.dead {
				if !strings.Contains(d, "synthetic.go:") {
					t.Errorf("diagnostic must name the source position, got: %s", d)
				}
			}
		})
	}
}
