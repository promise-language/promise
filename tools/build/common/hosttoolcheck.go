package common

import (
	"fmt"
	"regexp"
	"strings"
)

// The build takes no toolchain binary from the host (T2108). This file is what
// keeps that true: a structural guard over the tracked Go sources, named in
// structuralChecks beside its three siblings so bin/verify runs it, and run by
// the tools test suite against this very tree.
//
// It exists because the rule previously lived only in a commit message and a
// code comment. T2108 removed the PATH fallback from the LLVM and llvm-dlltool
// resolvers but left three winlink tests gating on `Which("llvm-dlltool")`, so
// the suite passed on a machine without LLVM and failed on one with it —
// Windows trunk went red on the only host that runs the check, and nothing in
// the build could tell anyone why (T2116). A rule with no enforcement is a rule
// that holds until the next person does not know it.
//
// The rule, stated once: every toolchain binary comes from the pinned
// prebuilts, an explicit PROMISE_* override, or a test's own stub. Never from
// PATH, Homebrew or Program Files — see docs/build-tools.md §4 and
// docs/runtime-architecture.md §"LLVM Tool Sources". Tests are bound by it too:
// a test that cannot get a tool from those sources skips for *that* reason, not
// because of what happens to be installed.

// hostToolOKMarker annotates a single host lookup as legitimate. Like
// sleepOKMarker and tempDirOKMarker it is per-line and must carry a reason: a
// file-level allowlist re-permits every future lookup in a file that earned its
// entry for one honest use, which is how the pattern survives a sweep.
const hostToolOKMarker = "// path-ok:"

// hostToolEnvironmentNames are the tools the build runs *inside* rather than
// builds *with*: the VCS, the POSIX shell and the Go toolchain itself. None of
// them can put bytes into a Promise artifact — go builds the compiler, not the
// programs it compiles — and pinning them is neither possible nor meaningful
// (bin/prereqs reports them). Looking one of these up needs no annotation.
var hostToolEnvironmentNames = map[string]bool{
	"git":   true,
	"sh":    true,
	"go":    true,
	"gofmt": true,
}

// toolchainCommandNames are binaries whose output is, or determines, a build
// artifact. Naming one of these bare to exec.Command resolves it through PATH
// exactly as Which does, so the guard covers that spelling too — otherwise the
// rule is bypassed by writing the lookup differently. Any `llvm-*` name counts.
var toolchainCommandNames = map[string]bool{
	"opt":        true,
	"llc":        true,
	"lld":        true,
	"ld.lld":     true,
	"ld64.lld":   true,
	"lld-link":   true,
	"wasm-ld":    true,
	"wasm-tools": true,
	"dlltool":    true,
	"clang":      true,
	"clang++":    true,
	"cc":         true,
	"gcc":        true,
	"ld":         true,
	"ar":         true,
	"ranlib":     true,
}

// isToolchainName reports whether a bare command name denotes a toolchain
// binary. The llvm- prefix covers the whole family (llvm-dlltool, llvm-ar, …)
// without listing it.
func isToolchainName(name string) bool {
	return toolchainCommandNames[name] || strings.HasPrefix(name, "llvm-")
}

// hostToolLookupArg matches a Which/LookPath call with a literal argument, so a
// lookup that names an environment tool can be told from one that does not, and
// from one whose argument is computed and therefore cannot be judged here.
var hostToolLookupArg = regexp.MustCompile(`(?:Which|LookPath)\("([^"]*)"\)`)

// execCommandArg matches exec.Command with a literal program name.
var execCommandArg = regexp.MustCompile(`exec\.Command\("([^"]*)"`)

// splitGoLine splits a Go source line into code and comment at the first `//`.
// Deliberately as blunt as the Promise guards' equivalent: a `//` inside a
// string literal truncates the code early, which can only ever hide a lookup
// written after one on the same line — a form that does not occur, and whose
// cost would be a missed report rather than a false one.
func splitGoLine(line string) (code, comment string) {
	if j := strings.Index(line, "//"); j >= 0 {
		return line[:j], line[j:]
	}
	return line, ""
}

// callCount counts calls to name in code, requiring a word boundary before it so
// `fooWhich(` is not one. A `func` immediately before the name is a declaration,
// not a call — `func Which(name string) string` defines the helper every other
// site here goes through, and flagging its signature would say the definition
// itself violates the rule.
func callCount(code, name string) int {
	n := 0
	for i := 0; ; {
		j := strings.Index(code[i:], name+"(")
		if j < 0 {
			return n
		}
		at := i + j
		if (at == 0 || !isIdentByte(code[at-1])) && !declaresAt(code, at) {
			n++
		}
		i = at + len(name) + 1
	}
}

// declaresAt reports whether the identifier starting at `at` is preceded by the
// `func` keyword (with only spaces between), i.e. this is its declaration.
func declaresAt(code string, at int) bool {
	head := strings.TrimRight(code[:at], " \t")
	if !strings.HasSuffix(head, "func") {
		return false
	}
	k := len(head) - len("func")
	return k == 0 || !isIdentByte(head[k-1])
}

// hostToolLookup reports whether a line's code resolves a program through the
// host's PATH in a way that needs justifying.
//
// A Which/LookPath whose every literal argument is an environment tool is not
// one. A computed argument always is: the guard cannot see what it resolves to,
// and "trust me" is precisely what the annotation is for.
func hostToolLookup(code string) bool {
	if calls := callCount(code, "Which") + callCount(code, "LookPath"); calls > 0 {
		lits := hostToolLookupArg.FindAllStringSubmatch(code, -1)
		if len(lits) != calls {
			return true // at least one argument is not a plain literal
		}
		for _, m := range lits {
			if !hostToolEnvironmentNames[m[1]] {
				return true
			}
		}
	}
	for _, m := range execCommandArg.FindAllStringSubmatch(code, -1) {
		if isToolchainName(m[1]) {
			return true
		}
	}
	return false
}

// hostToolLookupLines returns the 1-indexed line numbers of every line in data
// that resolves a program through PATH without a `// path-ok: <reason>` marker.
func hostToolLookupLines(data []byte) []int {
	var lines []int
	for i, line := range strings.Split(string(data), "\n") {
		code, comment := splitGoLine(line)
		if !hostToolLookup(code) {
			continue
		}
		if markedOK(comment, hostToolOKMarker) {
			continue // annotated with a reason — permitted
		}
		lines = append(lines, i+1)
	}
	return lines
}

// CheckHostToolLookups scans every tracked Go source and returns an error naming
// each line that resolves a program through the host's PATH without a reason.
//
// Tests are in scope on purpose. The bug this guard exists for was in a test:
// product code had already stopped consulting PATH while its test still gated on
// a PATH probe, and a guard that swept only non-test sources would have reported
// the tree clean while the trunk was red (T2116).
func CheckHostToolLookups(root string) error {
	// ANTLR writes compiler/internal/parser from the grammar, so nothing in it
	// is anyone's to annotate — the same tree, by the same predicate, that
	// bin/check keeps out of its findings.
	inScope := func(rel string) bool { return !isGeneratedGoFile(rel) }
	violations, err := scanTracked(root, "*.go", inScope, hostToolLookupLines)
	if err != nil {
		return err
	}
	if len(violations) > 0 {
		return fmt.Errorf("Go sources resolve a program through the host's PATH "+
			"(T2108/T2116 — the build and its tests take no toolchain binary from the host; it comes "+
			"from the pinned prebuilts, an explicit PROMISE_* override, or a test's own stub):\n%s\n"+
			"If a site genuinely reports host state (bin/prereqs, promise doctor), gates on a documented "+
			"test runtime, or serves an explicitly requested non-default target, annotate that line with "+
			"`%s <why>`.",
			strings.Join(violations, "\n"), hostToolOKMarker)
	}
	return nil
}
