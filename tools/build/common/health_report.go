package common

import (
	"regexp"
	"strconv"
	"strings"
)

// parseElapsed converts a duration string like "0.004s" to seconds as float64.
func parseElapsed(s string) float64 {
	s = strings.TrimSuffix(s, "s")
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

type testResult struct {
	File    string  `json:"file"`
	Test    string  `json:"test"`
	Outcome string  `json:"outcome"`
	Elapsed float64 `json:"elapsed"`
	Context string  `json:"context,omitempty"`
}

var resultRe = regexp.MustCompile(`^(pass|FAIL|TIMEOUT|LEAK)\s+\(([^)]+)\)\s+(.+)$`)

// ParseTestEntries parses promise test output into GateTestEntry values.
// target is the platform string (e.g. "linux-amd64", "wasm32-wasi").
func ParseTestEntries(target, output string) []GateTestEntry {
	raw := parseTestOutput(output)
	entries := make([]GateTestEntry, 0, len(raw))
	for _, r := range raw {
		entries = append(entries, GateTestEntry{
			Target:  target,
			File:    r.File,
			Test:    r.Test,
			Outcome: r.Outcome,
			Elapsed: r.Elapsed,
			Context: r.Context,
		})
	}
	return entries
}

// splitFileAndKind strips trailing " (...)" and " [...]" suffixes from a
// multi-file result line tail, returning the cleaned file path plus the
// rightmost stripped "(...)" group as `kind`. The "[target]" suffix is
// discarded — the caller already knows the target.
func splitFileAndKind(name string) (file, kind string) {
	file = name
	for {
		if strings.HasSuffix(file, "]") {
			if idx := strings.LastIndex(file, " ["); idx != -1 {
				file = strings.TrimSpace(file[:idx])
				continue
			}
		}
		if strings.HasSuffix(file, ")") {
			if idx := strings.LastIndex(file, " ("); idx != -1 {
				if kind == "" {
					kind = file[idx+2 : len(file)-1]
				}
				file = strings.TrimSpace(file[:idx])
				continue
			}
		}
		break
	}
	return
}

// refineFailOutcome maps the parenthesized kind from a multi-file failure
// line ("(1 timed out)", "(memory limit exceeded)", "(N/M leaked)", …) to
// the canonical outcome: FAIL / TIMEOUT / LEAK / MEMLIMIT.
func refineFailOutcome(kind string) string {
	switch {
	case strings.Contains(kind, "timed out"),
		strings.Contains(kind, "compilation timeout"):
		return "TIMEOUT"
	case strings.Contains(kind, "memory limit"):
		return "MEMLIMIT"
	case strings.Contains(kind, "leaked"):
		return "LEAK"
	default:
		return "FAIL"
	}
}

// parseTestOutput parses promise test output into result entries.
//
// Multi-file mode (file paths containing ".pr") always emits file-level
// entries with an empty `Test` field — the failure kind goes in `Outcome`,
// and per-test names / panic / leak / timeout / memlimit detail are folded
// into `Context`. This keeps the test identity (target, file, test) stable
// across passing and failing runs (T0742). Single-file mode keeps per-test
// names since the runner emits one line per `test` function.
func parseTestOutput(output string) []testResult {
	var results []testResult
	lines := strings.Split(output, "\n")

	for i := 0; i < len(lines); i++ {
		line := lines[i]

		m := resultRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		outcome := m[1]
		elapsed := m[2]
		name := m[3]

		if strings.Contains(name, ".pr") {
			// Multi-file mode: name is a file path with optional " (kind)"
			// and " [target]" suffixes. Identity is always file-level — never
			// stuff failure description into Test/File/Target. T0742.
			file, kind := splitFileAndKind(name)
			refined := outcome
			if outcome != "pass" {
				refined = refineFailOutcome(kind)
			}
			var ctx []string
			for i+1 < len(lines) && len(lines[i+1]) > 0 &&
				(lines[i+1][0] == ' ' || lines[i+1][0] == '\t') {
				i++
				ctx = append(ctx, strings.TrimSpace(lines[i]))
			}
			r := testResult{
				File:    file,
				Outcome: refined,
				Elapsed: parseElapsed(elapsed),
			}
			if len(ctx) > 0 {
				r.Context = strings.Join(ctx, "\n")
			}
			results = append(results, r)
			continue
		}

		// Single-file mode: name is a test function name.
		r := testResult{
			Test:    name,
			Outcome: outcome,
			Elapsed: parseElapsed(elapsed),
		}
		var ctx []string
		for i+1 < len(lines) && len(lines[i+1]) > 0 && lines[i+1][0] == ' ' {
			i++
			ctx = append(ctx, strings.TrimSpace(lines[i]))
		}
		if len(ctx) > 0 {
			r.Context = strings.Join(ctx, "\n")
		}
		results = append(results, r)
	}

	return results
}
