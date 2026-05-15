package common

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Baseline represents a single ratcheted metric. Three states:
//   - Enforced: Direction != "" && Value != nil — ratchet-checked
//   - Pending:  Direction != "" && Value == nil — value auto-populated on next run
//   - Informational: Type == "informational" — tracked but not enforced
type Baseline struct {
	Value     *float64 `json:"value,omitempty"`     // nil = not yet set (Pending)
	Direction string   `json:"direction,omitempty"` // "up", "down", "exact"; absent = Informational
	Updated   string   `json:"updated,omitempty"`   // YYYY-MM-DD
	Type      string   `json:"type,omitempty"`      // "informational" for auto-created entries
}

// Baselines maps platform → metric name → baseline.
type Baselines map[string]map[string]Baseline

const baselinesFile = "tools/gates/baselines.json"

// LoadBaselines reads baselines.json from the repo root.
func LoadBaselines(root string) (Baselines, error) {
	path := filepath.Join(root, baselinesFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read baselines: %w", err)
	}
	var b Baselines
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("parse baselines: %w", err)
	}
	return b, nil
}

// SaveBaselines writes baselines.json back with sorted keys for stable diffs.
func SaveBaselines(root string, b Baselines) error {
	path := filepath.Join(root, baselinesFile)
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal baselines: %w", err)
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

// checkRatchet returns true if the new value satisfies the ratchet direction.
func checkRatchet(direction string, baseline, actual float64) bool {
	switch direction {
	case "up":
		return actual >= baseline
	case "down":
		return actual <= baseline
	case "exact":
		return actual == baseline
	default:
		return true
	}
}

// trackerHTTPClient is the HTTP client used to query tracker REST endpoints.
var trackerHTTPClient = &http.Client{Timeout: 5 * time.Second}

// trackerURLOverride lets tests inject a tracker base URL without a .mcp.json file.
var trackerURLOverride = ""

// findTrackerURL returns the tracker base URL (with `/mcp` suffix stripped) by
// reading `.mcp.json` at the given root. Returns "" if the file is missing or
// does not declare a tracker server. Tests may set trackerURLOverride to bypass.
func findTrackerURL(root string) string {
	if trackerURLOverride != "" {
		return trackerURLOverride
	}
	data, err := os.ReadFile(filepath.Join(root, ".mcp.json"))
	if err != nil {
		return ""
	}
	var cfg struct {
		MCPServers map[string]struct {
			URL string `json:"url"`
		} `json:"mcpServers"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return ""
	}
	srv, ok := cfg.MCPServers["tracker"]
	if !ok || srv.URL == "" {
		return ""
	}
	return strings.TrimSuffix(srv.URL, "/mcp")
}

// queryGateExceptions fetches active (non-expired) exceptions for a (gate, metric)
// pair. Returns nil on any error (fail-closed: tracker unreachable means no
// exceptions, so regressions still fail the gate).
func queryGateExceptions(trackerURL, gate, metric string) []GateException {
	if trackerURL == "" {
		return nil
	}
	q := url.Values{}
	q.Set("gate", gate)
	q.Set("metric", metric)
	q.Set("include_expired", "false")
	resp, err := trackerHTTPClient.Get(trackerURL + "/api/gate-exceptions?" + q.Encode())
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	var out []GateException
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return nil
	}
	return out
}

// matchExceptionPlatform returns true if the exception's platform field matches
// the current OS. The "*" wildcard matches any platform.
func matchExceptionPlatform(exceptionPlatform, currentOS string) bool {
	return exceptionPlatform == "*" || exceptionPlatform == currentOS
}

// findMatchingException returns the first non-expired exception whose platform
// matches currentOS, or nil if none is found.
func findMatchingException(exceptions []GateException, currentOS string) *GateException {
	for i := range exceptions {
		if matchExceptionPlatform(exceptions[i].Platform, currentOS) {
			return &exceptions[i]
		}
	}
	return nil
}

// ratchetVerb returns a human-readable description of the ratchet direction.
func ratchetVerb(direction string) string {
	switch direction {
	case "up":
		return "must not decrease"
	case "down":
		return "must not increase"
	case "exact":
		return "must equal"
	default:
		return "unknown direction"
	}
}

// CheckCommitGate reads gate values and compares against baselines.
// Returns nil if all metrics pass. Updates baselines.json on improvements.
func CheckCommitGate(root string) error {
	// 1. Read gate values.
	gv, err := ReadGateValues(root, 10*time.Minute)
	if err != nil {
		return err
	}

	// 2. Determine current platform.
	platform := runtime.GOOS + "-" + runtime.GOARCH

	// 3. Load baselines.
	baselines, err := LoadBaselines(root)
	if err != nil {
		return err
	}

	platformBaselines, ok := baselines[platform]
	if !ok {
		platformBaselines = make(map[string]Baseline)
		baselines[platform] = platformBaselines
	}

	// 4. Auto-register unknown gate values as informational.
	changed := false
	for key := range gv.Values {
		if _, exists := platformBaselines[key]; !exists {
			platformBaselines[key] = Baseline{Type: "informational"}
			fmt.Printf("commit gate: new metric %q = %v — added as informational (set direction to enforce)\n",
				key, gv.Values[key])
			changed = true
		}
	}

	// 5. Check each metric.
	type regression struct {
		metric    string
		baseline  float64
		actual    float64
		direction string
	}
	type improvement struct {
		metric   string
		baseline float64
		actual   float64
	}

	var regressions []regression
	var improvements []improvement
	today := time.Now().Format("2006-01-02")

	// Sort metric names for deterministic output.
	metricNames := make([]string, 0, len(platformBaselines))
	for name := range platformBaselines {
		metricNames = append(metricNames, name)
	}
	sort.Strings(metricNames)

	for _, name := range metricNames {
		bl := platformBaselines[name]

		// Informational — skip.
		if bl.Type == "informational" {
			continue
		}

		// Look up the actual value from gate values.
		actual, found := gv.Values[name]
		if !found {
			continue
		}

		// Pending — auto-populate value.
		if bl.Direction != "" && bl.Value == nil {
			val := actual
			bl.Value = &val
			bl.Updated = today
			platformBaselines[name] = bl
			fmt.Printf("commit gate: pending metric %q populated: %v\n", name, actual)
			changed = true
			continue
		}

		// Enforced — ratchet check.
		if bl.Value != nil {
			if !checkRatchet(bl.Direction, *bl.Value, actual) {
				regressions = append(regressions, regression{
					metric:    name,
					baseline:  *bl.Value,
					actual:    actual,
					direction: bl.Direction,
				})
			} else if actual != *bl.Value {
				improvements = append(improvements, improvement{
					metric:   name,
					baseline: *bl.Value,
					actual:   actual,
				})
			}
		}
	}

	// 6. Consult tracker gate-exceptions, splitting regressions into excepted
	// (covered by an active exception) and hardFailures (must fail the gate).
	type exceptedRegression struct {
		regression
		exception GateException
	}
	var hardFailures []regression
	var excepted []exceptedRegression
	if len(regressions) > 0 {
		trackerURL := findTrackerURL(root)
		for _, r := range regressions {
			excs := queryGateExceptions(trackerURL, "commitgate", r.metric)
			if match := findMatchingException(excs, runtime.GOOS); match != nil {
				excepted = append(excepted, exceptedRegression{regression: r, exception: *match})
			} else {
				hardFailures = append(hardFailures, r)
			}
		}
	}

	// 7. Report excepted regressions (non-fatal warnings).
	for _, e := range excepted {
		ref := e.exception.BugID
		if ref == "" {
			ref = "no-bug"
		}
		reason := e.exception.Reason
		if reason == "" {
			reason = "no reason given"
		}
		fmt.Printf("commit gate: excepted regression (%s: %s):\n", ref, reason)
		fmt.Printf("  %s: %v → %v (%s, baseline: %v)\n",
			e.metric, e.baseline, e.actual, ratchetVerb(e.direction), e.baseline)
	}

	// 8. Report hard failures.
	if len(hardFailures) > 0 {
		fmt.Println("COMMIT GATE FAILED — quality regression detected:")
		for _, r := range hardFailures {
			fmt.Printf("  %s: %v → %v (%s, baseline: %v)\n",
				r.metric, r.baseline, r.actual, ratchetVerb(r.direction), r.baseline)
		}
		return fmt.Errorf("%d metric(s) regressed", len(hardFailures))
	}

	// 9. Report and apply improvements.
	if len(improvements) > 0 {
		fmt.Println("commit gate: baselines improved:")
		for _, imp := range improvements {
			fmt.Printf("  %s: %v → %v\n", imp.metric, imp.baseline, imp.actual)
			bl := platformBaselines[imp.metric]
			val := imp.actual
			bl.Value = &val
			bl.Updated = today
			platformBaselines[imp.metric] = bl
		}
		changed = true
	}

	if changed {
		baselines[platform] = platformBaselines
		if err := SaveBaselines(root, baselines); err != nil {
			return fmt.Errorf("update baselines: %w", err)
		}
		fmt.Printf("commit gate: updated %s — stage tools/gates/baselines.json with your commit\n", baselinesFile)
	} else if len(excepted) == 0 {
		fmt.Println("commit gate: all metrics match baselines — OK")
	}

	return nil
}
