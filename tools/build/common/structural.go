package common

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// isTestPrFile reports whether a repo-relative path is a Promise test file —
// either any .pr file under tests/ or any file whose name ends in _test.pr
// (module tests under modules/).
func isTestPrFile(rel string) bool {
	rel = filepath.ToSlash(rel)
	if strings.HasSuffix(rel, ".pr") && strings.HasPrefix(rel, "tests/") {
		return true
	}
	return strings.HasSuffix(rel, "_test.pr")
}

// sleepOKMarker annotates a single sleep() call as legitimate. It is the only
// escape hatch from the T1632 guard, and it is deliberately per-line rather than
// per-file: a file-level allowlist re-permits every future sleep in a file that
// earned its entry for one call, which is exactly how the pattern crept back in
// the first place. The marker must carry a reason.
const sleepOKMarker = "// sleep-ok:"

// isIdentByte reports whether b can appear inside a Promise identifier.
func isIdentByte(b byte) bool {
	return b == '_' ||
		(b >= '0' && b <= '9') ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z')
}

// containsSleepCall reports whether code calls sleep(). It requires a word
// boundary before the name, so an identifier that merely ends in "sleep" is not
// a call — `test_sleep()` declares a test, it does not synchronize on one.
func containsSleepCall(code string) bool {
	for i := 0; ; {
		j := strings.Index(code[i:], "sleep(")
		if j < 0 {
			return false
		}
		at := i + j
		if at == 0 || !isIdentByte(code[at-1]) {
			return true
		}
		i = at + len("sleep(")
	}
}

// sleepCallLines returns the 1-indexed line numbers of every sleep() call in
// data that is not annotated with a non-empty `// sleep-ok: <reason>` marker on
// the same line. A sleep( that appears only inside a comment is not a call.
func sleepCallLines(data []byte) []int {
	var lines []int
	for i, line := range strings.Split(string(data), "\n") {
		code, comment := line, ""
		if j := strings.Index(line, "//"); j >= 0 {
			code, comment = line[:j], line[j:]
		}
		if !containsSleepCall(code) {
			continue
		}
		if markedOK(comment, sleepOKMarker) {
			continue // annotated with a reason — permitted
		}
		lines = append(lines, i+1)
	}
	return lines
}

// tempDirOKMarker annotates a single temp_dir use as legitimate. Like
// sleepOKMarker it is deliberately per-line: a file-level allowlist would
// re-permit every future fixed path in a file that earned its entry for one
// honest use. The marker must carry a reason.
const tempDirOKMarker = "// temp-dir-ok:"

// markedOK reports whether a line's comment carries marker followed by a
// non-empty reason. Every structural guard in this file works this way, so the
// rule that a bare marker does not excuse anything is written once.
func markedOK(comment, marker string) bool {
	k := strings.Index(comment, marker)
	return k >= 0 && strings.TrimSpace(comment[k+len(marker):]) != ""
}

// containsIdent reports whether code contains name as a whole identifier. Both
// boundaries are required, so `os.temp_dir` is a hit (the leading '.' is not an
// identifier byte) while `test_temp_dir_x` and `_temp_dir_lookup` are not.
func containsIdent(code, name string) bool {
	for i := 0; ; {
		j := strings.Index(code[i:], name)
		if j < 0 {
			return false
		}
		at := i + j
		end := at + len(name)
		if (at == 0 || !isIdentByte(code[at-1])) &&
			(end == len(code) || !isIdentByte(code[end])) {
			return true
		}
		i = end
	}
}

// splitPrLine splits a Promise source line into its code and comment parts,
// with string- and char-literal *contents* removed from the code. inBlock says
// whether the line starts inside a """ block; the returned bool says whether it
// ends inside one. Stripping literals is what keeps the word "temp_dir" inside
// an assertion message or a snapshot's expected output from reading as a use.
//
// A `{…}` interpolation inside a regular string is the exception: it *is* code,
// and `"{os.temp_dir}/pr_x"` is as idiomatic a spelling of the shared-path bug
// as the concatenated form, so its expression is emitted (space-delimited, so
// two adjacent interpolations cannot fuse into one identifier). Triple-quoted
// and raw strings do not interpolate (PromiseLexer.g4), so their contents stay
// stripped; `\{` is an escape, not an interpolation.
func splitPrLine(line string, inBlock bool) (code, comment string, stillInBlock bool) {
	var b strings.Builder
	for i := 0; i < len(line); {
		if inBlock {
			j := strings.Index(line[i:], `"""`)
			if j < 0 {
				return b.String(), "", true
			}
			i += j + 3
			inBlock = false
			continue
		}
		switch {
		case strings.HasPrefix(line[i:], "//"):
			return b.String(), line[i:], false
		case strings.HasPrefix(line[i:], `"""`):
			i += 3
			inBlock = true
		case line[i] == 'r' && i+1 < len(line) && line[i+1] == '"' &&
			(i == 0 || !isIdentByte(line[i-1])):
			// Raw string: no escapes and no interpolation (RAW_STRING in
			// PromiseLexer.g4 is 'r"' ~["]* '"'), so the next quote closes it.
			i += 2
			if j := strings.IndexByte(line[i:], '"'); j >= 0 {
				i += j + 1
			} else {
				i = len(line)
			}
		case line[i] == '"':
			i = skipQuoted(line, i, '"', &b)
		case line[i] == '\'':
			i = skipQuoted(line, i, '\'', nil)
		default:
			b.WriteByte(line[i])
			i++
		}
	}
	return b.String(), "", inBlock
}

// skipQuoted advances past the literal opening at line[at] with the given quote
// byte and returns the index just past its close (or len(line) if the literal is
// unterminated). When interp is non-nil, the text of each `{…}` interpolation is
// written to it, surrounded by spaces; brace depth is tracked so a nested string
// or a nested `{}` cannot end the expression early.
func skipQuoted(line string, at int, quote byte, interp *strings.Builder) int {
	i := at + 1
	for i < len(line) && line[i] != quote {
		switch {
		case line[i] == '\\':
			i += 2 // an escape consumes the byte after the backslash
		case line[i] == '{' && interp != nil:
			interp.WriteByte(' ')
			depth := 1
			i++
			for i < len(line) && depth > 0 {
				switch line[i] {
				case '\\':
					i++ // fall through to the trailing i++ for the escaped byte
				case '{':
					depth++
				case '}':
					depth--
				case '"':
					// A nested literal's braces belong to it, not to us.
					i = skipQuoted(line, i, '"', nil) - 1
				}
				if depth > 0 && i < len(line) {
					interp.WriteByte(line[i])
				}
				i++
			}
			interp.WriteByte(' ')
		default:
			i++
		}
	}
	return i + 1
}

// tempDirLines returns the 1-indexed line numbers of every line in data whose
// code names temp_dir without also naming process_id and without a non-empty
// `// temp-dir-ok: <reason>` marker. The rule is deliberately blunt, like the
// sleep guard: it flags the hoisted form (`string d = os.temp_dir;`) that a
// narrower "only flag temp_dir + \"…\"" rule would miss.
func tempDirLines(data []byte) []int {
	var lines []int
	inBlock := false
	for i, line := range strings.Split(string(data), "\n") {
		var code, comment string
		code, comment, inBlock = splitPrLine(line, inBlock)
		if !containsIdent(code, "temp_dir") {
			continue
		}
		if containsIdent(code, "process_id") {
			continue // per-process by construction — the point of the rule
		}
		if markedOK(comment, tempDirOKMarker) {
			continue // annotated with a reason — permitted
		}
		lines = append(lines, i+1)
	}
	return lines
}

// scanTracked walks every tracked file matching glob that the scope predicate
// admits and returns one "  path:line" entry per line that flag reports, in
// index order. It reads the full index rather than the staged set so a violation
// committed on a previous turn is caught on the next invocation — the property
// every guard built on it depends on.
func scanTracked(root, glob string, scope func(string) bool, flag func([]byte) []int) ([]string, error) {
	out, err := RunOutputIn(root, "git", "ls-files", "-z", glob)
	if err != nil {
		return nil, fmt.Errorf("list tracked %s files: %w", glob, err)
	}

	// git ls-files emits paths in sorted index order, and each flag function
	// returns line numbers in ascending order, so entries come out already ordered.
	var violations []string
	for rel := range strings.SplitSeq(out, "\x00") {
		if rel == "" {
			continue
		}
		slashRel := filepath.ToSlash(rel)
		if !scope(slashRel) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			// Tracked but absent from the worktree (staged deletion). Skip.
			continue
		}
		for _, line := range flag(data) {
			violations = append(violations, fmt.Sprintf("  %s:%d", slashRel, line))
		}
	}
	return violations, nil
}

// CheckTestTempPaths scans all tracked Promise test files and examples and
// returns an error naming every temp_dir use that is not unique per process.
func CheckTestTempPaths(root string) error {
	// Examples are run as part of the suite (tools/build/common/test.go), so a
	// fixed path there collides exactly as one in a test file does.
	inScope := func(rel string) bool {
		return isTestPrFile(rel) || strings.HasPrefix(rel, "examples/")
	}
	violations, err := scanTracked(root, "*.pr", inScope, tempDirLines)
	if err != nil {
		return err
	}
	if len(violations) > 0 {
		return fmt.Errorf("test files build scratch paths from temp_dir without a per-process name "+
			"(T1963 — os.temp_dir is machine-wide, so a fixed name is shared by every concurrent "+
			"run on the host; include os.process_id):\n%s\n"+
			"If a line genuinely uses temp_dir itself rather than naming a scratch file, "+
			"annotate it with `%s <why>`.",
			strings.Join(violations, "\n"), tempDirOKMarker)
	}
	return nil
}

// CheckTestSleeps scans all tracked Promise test files and returns an error
// naming every unannotated sleep() call site.
func CheckTestSleeps(root string) error {
	violations, err := scanTracked(root, "*.pr", isTestPrFile, sleepCallLines)
	if err != nil {
		return err
	}
	if len(violations) > 0 {
		return fmt.Errorf("test files synchronize on sleep() (T1632 — order concurrent operations "+
			"with a channel, a ready handshake or an awaited task):\n%s\n"+
			"If a call genuinely measures time rather than ordering two concurrent operations, "+
			"annotate that line with `%s <why>`.",
			strings.Join(violations, "\n"), sleepOKMarker)
	}
	return nil
}

// structuralChecks is the one list of structural sweeps this project enforces
// over its own sources. It is a list rather than four call sites because a
// check's caller is the thing that goes missing: every one of these was written
// as a pre-commit check, the hook stopped naming the tool that ran them, and two
// of them then held by review alone for as long as nobody noticed (T2160). A
// check named here gets both its bin/verify caller and its real-tree test in the
// tools suite; a check absent from it has neither, and that is now one fact to
// look at instead of two.
//
// The name is what a failure is reported under, so it matches the guard's own
// vocabulary rather than the Go identifier.
var structuralChecks = []struct {
	name  string
	check func(root string) error
}{
	{"docs", CheckDocs},
	{"test-sleeps", CheckTestSleeps},
	{"test-temp-paths", CheckTestTempPaths},
	{"host-tool-lookups", CheckHostToolLookups},
}

// RunStructuralChecks runs every sweep in structuralChecks over the tracked
// sources at root and reports all of their findings together.
//
// None of them reads a staged set — each reads the working tree, scoped by
// `git ls-files` where it needs to skip untracked and generated output. That is
// what lets them run outside a commit at all: they answer "is this tree clean",
// not "is this commit clean", so a violation committed on a previous turn is
// still caught on the next run.
//
// All four run even after one fails. They are independent sweeps over disjoint
// file sets, and a run that stopped at the first would turn one fix-and-rerun
// cycle into four — the same reason CheckDocs already aggregates its own halves.
func RunStructuralChecks(root string) error {
	var problems []error
	for _, c := range structuralChecks {
		if err := c.check(root); err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", c.name, err))
		}
	}
	return errors.Join(problems...)
}
