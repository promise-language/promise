package common

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type testResult struct {
	File    string `json:"file"`
	Test    string `json:"test"`
	Outcome string `json:"outcome"`
	Elapsed string `json:"elapsed"`
	Context string `json:"context,omitempty"`
}

type testReport struct {
	Commit  string       `json:"commit"`
	Host    string       `json:"host"`
	Target  string       `json:"target"`
	Clean   bool         `json:"clean"`
	Pushed  bool         `json:"pushed"`
	Results []testResult `json:"results"`
}

var resultRe = regexp.MustCompile(`^(pass|FAIL|TIMEOUT|LEAK)\s+\(([^)]+)\)\s+(.+)$`)
var fileSuffixRe = regexp.MustCompile(`\s+(\(.*?\)|\[.*?\])$`)

// ReportTestHealth parses test output, checks git preconditions, and POSTs
// results to the tracker health API. Fire-and-forget: errors are logged to
// stderr and never fail the build.
func ReportTestHealth(root, target, output string) {
	// 1. Git preconditions.
	clean, pushed := checkGitPreconditions(root)
	if !clean {
		fmt.Fprintf(os.Stderr, "health: skipping, dirty worktree\n")
		return
	}
	if !pushed {
		fmt.Fprintf(os.Stderr, "health: skipping, commit not pushed\n")
		return
	}

	// 2. Collect metadata.
	commit := gitOutput(root, "rev-parse", "HEAD")
	if commit == "" {
		fmt.Fprintf(os.Stderr, "health: skipping, cannot determine commit\n")
		return
	}
	host, _ := os.Hostname()

	// 3. Parse test output.
	results := parseTestOutput(output)
	if len(results) == 0 {
		return
	}

	// 4. Find tracker URL.
	trackerURL := findTrackerURL(root)
	if trackerURL == "" {
		fmt.Fprintf(os.Stderr, "health: no tracker URL found\n")
		return
	}

	// 5. POST report.
	report := testReport{
		Commit:  commit,
		Host:    host,
		Target:  target,
		Clean:   clean,
		Pushed:  pushed,
		Results: results,
	}
	postReport(trackerURL, report)
}

// checkGitPreconditions returns (clean, pushed).
func checkGitPreconditions(root string) (bool, bool) {
	// Check dirty worktree.
	status := gitOutput(root, "status", "--porcelain")
	if status != "" {
		return false, false
	}
	// Check commit is pushed.
	cmd := exec.Command("git", "-C", root, "merge-base", "--is-ancestor", "HEAD", "@{u}")
	if err := cmd.Run(); err != nil {
		return true, false
	}
	return true, true
}

// gitOutput runs a git command and returns trimmed stdout, or "" on error.
func gitOutput(root string, args ...string) string {
	fullArgs := append([]string{"-C", root}, args...)
	cmd := exec.Command("git", fullArgs...)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
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
						Elapsed: elapsed,
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
							Elapsed: elapsed,
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
					Elapsed: elapsed,
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

// findTrackerURL returns the tracker base URL from env or .mcp.json.
func findTrackerURL(root string) string {
	if url := os.Getenv("PROMISE_TRACKER_URL"); url != "" {
		return strings.TrimRight(url, "/")
	}

	mcpPath := filepath.Join(root, ".mcp.json")
	data, err := os.ReadFile(mcpPath)
	if err != nil {
		return ""
	}

	var config struct {
		MCPServers map[string]struct {
			URL string `json:"url"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return ""
	}

	tracker, ok := config.MCPServers["tracker"]
	if !ok || tracker.URL == "" {
		return ""
	}

	// Strip /mcp suffix to get base URL.
	base := strings.TrimRight(tracker.URL, "/")
	base = strings.TrimSuffix(base, "/mcp")
	return base
}

// postReport sends the test report to the tracker health API.
func postReport(trackerURL string, report testReport) {
	body, err := json.Marshal(report)
	if err != nil {
		fmt.Fprintf(os.Stderr, "health: marshal error: %v\n", err)
		return
	}

	client := &http.Client{Timeout: 5 * time.Second}
	url := trackerURL + "/api/health/report"

	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "health: POST %s: %v\n", url, err)
		return
	}
	resp.Body.Close()

	if resp.StatusCode >= 300 {
		fmt.Fprintf(os.Stderr, "health: POST %s: status %d\n", url, resp.StatusCode)
	}
}
