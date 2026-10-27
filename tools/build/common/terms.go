package common

// The terms bin/run judges by, and nothing that moves them.
//
// Two files, moved for different reasons by different hands: caps in
// tools/gates/thresholds.json, which a person edits, and baselines in
// tools/gates/baselines.json, the best a metric has been. Both are versioned
// with the tree they judge, so a commit carries the terms it was judged on and
// any machine reaches the same verdict for it, offline. Advancing a baseline is
// the workspace's job; nothing in this repository writes either file.
//
// THE READING IS STRICT. An unknown key, a missing field, an unknown direction,
// a baseline giving both one value and per-target values, a metric no gate
// declares, and one metric given two directions are each refused, naming the
// file and the entry. A term the judge silently skipped would be a rule nobody
// is held to while the file says otherwise.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Direction is the sense in which a measurement is compared to a term. Both are
// inclusive bounds. The set is closed.
type Direction string

const (
	AtMost  Direction = "at_most"  // value must not exceed the term (counts of bad things)
	AtLeast Direction = "at_least" // value must not fall below the term (floors such as free space)
)

// ThresholdsFile and BaselinesFile are the two terms files, repo-relative. They
// are distinct artefacts from the gates on purpose: a gate holding its own terms
// can be passed by editing the gate.
var (
	ThresholdsFile = filepath.Join("tools", "gates", "thresholds.json")
	BaselinesFile  = filepath.Join("tools", "gates", "baselines.json")
)

// capTerm is one person-edited bound.
type capTerm struct {
	Direction Direction
	Cap       json.Number
}

// baselineTerm is one ratcheted bound: one value for every target, or one per
// target for a metric that differs by platform — never both.
type baselineTerm struct {
	Direction Direction
	Value     *json.Number
	Targets   map[string]json.Number
}

// at is the baseline that applies to target; ok is false when the metric has
// none there, which leaves it unjudged by this file on that target.
func (b baselineTerm) at(target string) (json.Number, bool) {
	if b.Value != nil {
		return *b.Value, true
	}
	v, ok := b.Targets[target]
	return v, ok
}

// Terms is both files, read.
type Terms struct {
	Caps      map[string]capTerm
	Baselines map[string]baselineTerm
}

// LoadTerms reads both terms files and checks them against every metric this
// project's gates declare. Either file absent is an error: a project with a
// judge must have both.
func LoadTerms(root string) (Terms, error) {
	// One file serves every platform, so a metric only another platform
	// measures is as known as one this host declares.
	declared := map[string]bool{unlinkMetricName: true}
	for _, name := range ContractGateNames() {
		for _, s := range declaredMetrics(root, name) {
			declared[s.Name] = true
		}
	}
	caps, err := readTermsFile(root, ThresholdsFile, declared, []string{"direction", "cap"}, nil)
	if err != nil {
		return Terms{}, err
	}
	t := Terms{Caps: map[string]capTerm{}, Baselines: map[string]baselineTerm{}}
	var baselines map[string]termEntry
	if legacy, ok, err := readPlatformFirstBaselines(root, declared); err != nil {
		return Terms{}, err
	} else if ok {
		t.Baselines = legacy
	} else if baselines, err = readTermsFile(root, BaselinesFile, declared, []string{"direction"}, []string{"value", "targets"}); err != nil {
		return Terms{}, err
	}
	var problems []string
	for _, name := range sortedMapKeys(caps) {
		e := caps[name]
		n, err := termNumber(e["cap"])
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %q: \"cap\" %v", ThresholdsFile, name, err))
			continue
		}
		t.Caps[name] = capTerm{Direction: e.direction(), Cap: n}
	}
	for _, name := range sortedMapKeys(baselines) {
		e := baselines[name]
		b := baselineTerm{Direction: e.direction()}
		value, hasValue := e["value"]
		targets, hasTargets := e["targets"]
		switch {
		case hasValue && hasTargets:
			problems = append(problems, fmt.Sprintf("%s: %q gives both \"value\" and \"targets\"; give one", BaselinesFile, name))
			continue
		case hasValue:
			n, err := termNumber(value)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: %q: \"value\" %v", BaselinesFile, name, err))
				continue
			}
			b.Value = &n
		case hasTargets:
			m, err := targetNumbers(targets)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: %q: \"targets\" %v", BaselinesFile, name, err))
				continue
			}
			b.Targets = m
		default:
			problems = append(problems, fmt.Sprintf("%s: %q has neither \"value\" nor \"targets\"", BaselinesFile, name))
			continue
		}
		if c, ok := t.Caps[name]; ok && c.Direction != b.Direction {
			problems = append(problems, fmt.Sprintf("%q is %s in %s and %s in %s; one metric has one direction",
				name, c.Direction, ThresholdsFile, b.Direction, BaselinesFile))
			continue
		}
		t.Baselines[name] = b
	}
	for _, name := range sortedMapKeys(t.Baselines) {
		if c, ok := t.Caps[name]; ok && c.Direction != t.Baselines[name].Direction && baselines == nil {
			problems = append(problems, fmt.Sprintf("%q is %s in %s and %s in %s; one metric has one direction",
				name, c.Direction, ThresholdsFile, t.Baselines[name].Direction, BaselinesFile))
		}
	}
	if len(problems) > 0 {
		return Terms{}, fmt.Errorf("invalid terms:\n  %s", strings.Join(problems, "\n  "))
	}
	return t, nil
}

// The platform-first shape is how baselines.json is written today, and the
// only one the workspace's commit guard reads: platform → metric → {value,
// direction: up | down | exact, updated} or {type: informational}. Its ratchet
// check refuses a staged file in any other shape, and refuses an enforced entry
// removed — so the file cannot move to the metric-first shape, nor follow a
// metric's rename, until the guard reads the shape this reader exists to
// replace (promise-language/workspace#490). Until then the judge reads it as it
// stands, through the conversion the tool contract fixes: `up` is at_least,
// `down` at_most, `exact 0` on a count at_most 0, and informational and pending
// entries are no term. Each platform's figure becomes that target's.

// retiredBaselineNames is each name the platform-first file still carries for a
// metric a gate now reports under another, or no longer reports at all ("").
// A name in neither this table nor any gate's declaration is refused, so a typo
// still cannot pass for a term.
var retiredBaselineNames = map[string]string{
	"go_test_failures":          "go_failed_tests",
	"go_test_packages_failed":   "go_failed_packages",
	"host_leak_count":           "promise_leaked_tests",
	"host_test_count":           "promise_test_count",
	"host_test_failures":        "promise_failed_tests",
	"promise_check_errors":      "promise_check_findings",
	"unbuildable_go_packages":   "go_unbuildable_packages",
	"unformatted_go_files":      "go_unformatted_files",
	"unformatted_promise_files": "promise_unformatted_files",
	"vet_findings":              "go_check_findings",
	"wasm_leak_count":           "wasm_leaked_tests",
	"wasm_test_failures":        "wasm_failed_tests",
	"wasm_web_leak_count":       "wasm_web_leaked_tests",
	"wasm_web_test_failures":    "wasm_web_failed_tests",
	// Folded into the failed_tests of their suite, which is capped at zero, or
	// measured by no gate: each judges nothing.
	"host_timeout_count": "", "host_memory_count": "", "host_not_run_count": "", "host_excluded_count": "",
	"wasm_timeout_count": "", "wasm_memory_count": "", "wasm_not_run_count": "", "wasm_excluded_count": "",
	"cas_materializations": "", "cas_materialized_bytes": "", "stale_generated_files": "",
	"go_coverage_pct": "", "promise_coverage_pct": "",
}

// platformKey is a platform-first block's key, os-arch, which names the target
// os/arch.
var platformKey = regexp.MustCompile(`^[a-z0-9]+-[a-z0-9]+$`)

// readPlatformFirstBaselines reads baselines.json when it is in the
// platform-first shape, and reports ok=false — reading nothing — when it is
// not, leaving it to the metric-first reader.
func readPlatformFirstBaselines(root string, declared map[string]bool) (map[string]baselineTerm, bool, error) {
	data, err := os.ReadFile(filepath.Join(root, BaselinesFile))
	if err != nil {
		return nil, false, nil // the metric-first reader names the missing file
	}
	var blocks map[string]map[string]map[string]json.RawMessage
	if json.Unmarshal(data, &blocks) != nil || len(blocks) == 0 {
		return nil, false, nil
	}
	for platform := range blocks {
		if !platformKey.MatchString(platform) || declared[platform] {
			return nil, false, nil
		}
	}
	out := map[string]baselineTerm{}
	var problems []string
	for _, platform := range sortedMapKeys(blocks) {
		target := strings.Replace(platform, "-", "/", 1)
		for _, name := range sortedMapKeys(blocks[platform]) {
			e := blocks[platform][name]
			where := fmt.Sprintf("%s: %s/%s", BaselinesFile, platform, name)
			metric := name
			if renamed, retired := retiredBaselineNames[name]; retired {
				metric = renamed
			} else if !declared[name] {
				problems = append(problems, where+" is a metric no gate declares")
				continue
			}
			for _, key := range sortedMapKeys(e) {
				if !slices.Contains([]string{"value", "direction", "updated", "type"}, key) {
					problems = append(problems, fmt.Sprintf("%s has an unknown key %q", where, key))
				}
			}
			var direction, kind string
			_ = json.Unmarshal(e["direction"], &direction)
			_ = json.Unmarshal(e["type"], &kind)
			raw, hasValue := e["value"]
			if metric == "" || kind == "informational" || !hasValue || direction == "" {
				continue // no term: informational, pending, or measured by no gate
			}
			value, err := termNumber(raw)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: \"value\" %v", where, err))
				continue
			}
			var d Direction
			switch direction {
			case "up":
				d = AtLeast
			case "down":
				d = AtMost
			case "exact":
				if value != "0" {
					problems = append(problems, where+" is exact on a non-zero value, which no inclusive bound states")
					continue
				}
				d = AtMost
			default:
				problems = append(problems, fmt.Sprintf("%s has an unknown direction %q (must be up, down or exact)", where, direction))
				continue
			}
			b, seen := out[metric]
			if !seen {
				b = baselineTerm{Direction: d, Targets: map[string]json.Number{}}
			} else if b.Direction != d {
				problems = append(problems, fmt.Sprintf("%s is %s here and %s on another platform; one metric has one direction", where, d, b.Direction))
				continue
			}
			b.Targets[target] = value
			out[metric] = b
		}
	}
	if len(problems) > 0 {
		return nil, true, fmt.Errorf("invalid terms:\n  %s", strings.Join(problems, "\n  "))
	}
	return out, true, nil
}

// termEntry is one entry of a terms file, by key, not yet typed.
type termEntry map[string]json.RawMessage

func (e termEntry) direction() Direction {
	var d Direction
	_ = json.Unmarshal(e["direction"], &d)
	return d
}

// readTermsFile reads one file's entries and holds each to its keys: every one
// of required present, nothing outside required and optional, a known direction,
// and a metric some gate declares. Every problem is reported, not the first.
func readTermsFile(root, file string, declared map[string]bool, required, optional []string) (map[string]termEntry, error) {
	data, err := os.ReadFile(filepath.Join(root, file))
	if err != nil {
		return nil, fmt.Errorf("loading %s: %w", file, err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", file, err)
	}
	entries := map[string]termEntry{}
	var problems []string
	for _, name := range sortedMapKeys(raw) {
		var e termEntry
		if err := json.Unmarshal(raw[name], &e); err != nil || e == nil {
			problems = append(problems, fmt.Sprintf("%s: %q is not an object", file, name))
			continue
		}
		if !declared[name] {
			problems = append(problems, fmt.Sprintf("%s: %q is a metric no gate declares", file, name))
		}
		for _, key := range sortedMapKeys(e) {
			if !slices.Contains(required, key) && !slices.Contains(optional, key) {
				problems = append(problems, fmt.Sprintf("%s: %q has an unknown key %q", file, name, key))
			}
		}
		for _, key := range required {
			if _, ok := e[key]; !ok {
				problems = append(problems, fmt.Sprintf("%s: %q is missing %q", file, name, key))
			}
		}
		if d, ok := e["direction"]; ok {
			var dir Direction
			if json.Unmarshal(d, &dir) != nil || (dir != AtMost && dir != AtLeast) {
				problems = append(problems, fmt.Sprintf("%s: %q has an unknown direction %s (must be %q or %q)", file, name, d, AtMost, AtLeast))
			}
		}
		entries[name] = e
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("invalid terms:\n  %s", strings.Join(problems, "\n  "))
	}
	return entries, nil
}

// termNumber reads one number, kept as written so an integer stays one.
func termNumber(raw json.RawMessage) (json.Number, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", fmt.Errorf("is not a number")
	}
	n, ok := v.(json.Number)
	if !ok {
		return "", fmt.Errorf("is not a number")
	}
	return n, nil
}

// targetNumbers reads a baseline's per-target values: an object whose every key
// is an os/arch target and every value a number.
func targetNumbers(raw json.RawMessage) (map[string]json.Number, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, fmt.Errorf("is not an object of target to number")
	}
	out := map[string]json.Number{}
	for target, v := range m {
		if os, arch, ok := strings.Cut(target, "/"); !ok || os == "" || arch == "" {
			return nil, fmt.Errorf("names %q, which is not an os/arch target", target)
		}
		n, err := termNumber(v)
		if err != nil {
			return nil, fmt.Errorf("for %s %v", target, err)
		}
		out[target] = n
	}
	return out, nil
}

// holds reports whether m is within bound in direction d. An integer metric is
// compared as an integer wherever the bound is one, so a count past 2^53 is
// never rounded into compliance.
func holds(m Metric, d Direction, bound json.Number) bool {
	if m.Type == MetricInt {
		if b, err := strconv.ParseInt(string(bound), 10, 64); err == nil {
			if d == AtLeast {
				return m.Int >= b
			}
			return m.Int <= b
		}
	}
	b, _ := strconv.ParseFloat(string(bound), 64)
	if d == AtLeast {
		return m.Number() >= b
	}
	return m.Number() <= b
}

// sortedMapKeys is the map's keys in a stable order, so problems and usage text
// read the same on every run.
func sortedMapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
