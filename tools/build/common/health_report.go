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
var fileSuffixRe = regexp.MustCompile(`\s+(\(.*?\)|\[.*?\])$`)

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

// parseTestOutput parses promise test output into result entries.
func parseTestOutput(output string) []testResult {
	var results []testResult
	lines := strings.Split(output, "\n")

	var currentFile string
	var currentOutcome string
	var compilationError bool

	for i := 0; i < len(lines); i++ {
		line := lines[i]

		// Match result lines: "pass (0.004s) e2e/basics.pr (3 tests)"
		if m := resultRe.FindStringSubmatch(line); m != nil {
			outcome := m[1]
			elapsed := m[2]
			name := m[3]

			// Detect multi-file vs single-file by .pr in name.
			if strings.Contains(name, ".pr") {
				// Multi-file mode: name is a file path.
				compilationError = strings.Contains(name, "(compilation error)")
				// Strip trailing suffixes like "(3 tests)", "[wasm32-wasi]".
				file := name
				for {
					trimmed := fileSuffixRe.ReplaceAllString(file, "")
					if trimmed == file {
						break
					}
					file = strings.TrimSpace(trimmed)
				}

				if outcome == "pass" {
					results = append(results, testResult{
						File:    file,
						Outcome: outcome,
						Elapsed: parseElapsed(elapsed),
					})
					currentFile = ""
					currentOutcome = ""
					compilationError = false
				} else {
					// FAIL/LEAK/TIMEOUT — details follow as indented lines.
					currentFile = file
					currentOutcome = outcome

					if compilationError {
						// Compilation errors: collect indented context lines.
						var ctx []string
						for i+1 < len(lines) && len(lines[i+1]) > 0 && lines[i+1][0] == ' ' {
							i++
							ctx = append(ctx, strings.TrimSpace(lines[i]))
						}
						results = append(results, testResult{
							File:    file,
							Outcome: outcome,
							Elapsed: parseElapsed(elapsed),
							Context: strings.Join(ctx, "\n"),
						})
						currentFile = ""
						currentOutcome = ""
						compilationError = false
					}
				}
			} else {
				// Single-file mode: name is a test function name.
				r := testResult{
					Test:    name,
					Outcome: outcome,
					Elapsed: parseElapsed(elapsed),
				}
				// Collect indented context lines.
				var ctx []string
				for i+1 < len(lines) && len(lines[i+1]) > 0 && lines[i+1][0] == ' ' {
					i++
					ctx = append(ctx, strings.TrimSpace(lines[i]))
				}
				if len(ctx) > 0 {
					r.Context = strings.Join(ctx, "\n")
				}
				results = append(results, r)
				currentFile = ""
				currentOutcome = ""
			}
			continue
		}

		// Indented lines after a multi-file FAIL/LEAK/TIMEOUT.
		if currentFile != "" && currentOutcome != "" && !compilationError {
			if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "    ") {
				// 2-space indent: test name.
				testName := strings.TrimSpace(line)
				// Collect 4-space context lines.
				var ctx []string
				for i+1 < len(lines) && strings.HasPrefix(lines[i+1], "    ") {
					i++
					ctx = append(ctx, strings.TrimSpace(lines[i]))
				}
				r := testResult{
					File:    currentFile,
					Test:    testName,
					Outcome: currentOutcome,
				}
				if len(ctx) > 0 {
					r.Context = strings.Join(ctx, "\n")
				}
				results = append(results, r)
			} else if !strings.HasPrefix(line, " ") {
				// Non-indented line: end of details for current file.
				currentFile = ""
				currentOutcome = ""
			}
		}
	}

	return results
}
