package common

import (
	"encoding/json"
	"fmt"
	"strings"
)

// blockedFiles lists compiled binaries that must never be committed.
var blockedFiles = []string{
	"compiler/promise",
	"compiler/promise.exe",
	"bin/promise",
	"bin/promise.exe",
}

// RunPreCommit implements the git pre-commit hook. It rejects commits that
// include compiled binaries and validates baselines.json ratchet direction.
func RunPreCommit(root string) error {
	// core.quotePath=false keeps non-ASCII bytes raw in the output (the
	// default would octal-escape them into an all-ASCII quoted string,
	// defeating the non-ASCII filename check below).
	out, err := RunOutputIn(root, "git", "-c", "core.quotePath=false", "diff", "--cached", "--name-only")
	if err != nil {
		return fmt.Errorf("list staged files: %w", err)
	}
	if out == "" {
		return nil
	}

	staged := strings.Split(out, "\n")
	for _, blocked := range blockedFiles {
		for _, f := range staged {
			if f == blocked {
				return fmt.Errorf("staged file '%s' is a compiled binary — remove it from the commit.\n  git reset HEAD %s", blocked, blocked)
			}
		}
	}

	// Reject stray log files and files whose names contain non-ASCII
	// characters — both are typically leftover temp artifacts (e.g. from
	// flow runs) that should never be committed.
	for _, f := range staged {
		if f == "" {
			continue
		}
		if strings.HasSuffix(f, ".log") {
			return fmt.Errorf("staged file '%s' is a .log file — log files must not be committed.\n  git reset HEAD %s", f, f)
		}
		if !isASCII(f) {
			return fmt.Errorf("staged file '%s' has a non-ASCII file name — rename it before committing.\n  git reset HEAD %s", f, f)
		}
	}

	// Defense-in-depth: if baselines.json is staged, verify no metric regressed
	// vs the committed (HEAD) version.
	for _, f := range staged {
		if f == baselinesFile {
			if err := validateBaselinesDiff(root); err != nil {
				return err
			}
			break
		}
	}

	// Reject commits when running the formatter would introduce changes —
	// otherwise unformatted code reaches origin and shows up as a spurious diff
	// the next time someone runs bin/verify (which reformats in place). Go is
	// checked in-process via go/format; Promise is checked by shelling out to
	// `bin/promise format -check` (the formatter lives in the compiler binary),
	// the same way bin/verify invokes it.
	var unformatted []string

	goFiles, err := UnformattedGoFiles(root)
	if err != nil {
		return fmt.Errorf("check Go formatting: %w", err)
	}
	unformatted = append(unformatted, goFiles...)

	prFiles, err := UnformattedPromiseFiles(root)
	if err != nil {
		return fmt.Errorf("check Promise formatting: %w", err)
	}
	unformatted = append(unformatted, prFiles...)

	if len(unformatted) > 0 {
		return fmt.Errorf("unformatted files — run bin/format and re-stage:\n  %s",
			strings.Join(unformatted, "\n  "))
	}

	return nil
}

// isASCII reports whether s contains only ASCII bytes (0x00–0x7F).
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 0x7F {
			return false
		}
	}
	return true
}

// validateBaselinesDiff compares staged baselines.json against HEAD and rejects
// any metric that moved in the wrong direction (e.g., test count decreased,
// leak count increased, exact metric changed).
func validateBaselinesDiff(root string) error {
	// Read HEAD version of baselines.json.
	headData, err := RunOutputIn(root, "git", "show", "HEAD:"+baselinesFile)
	if err != nil {
		// File doesn't exist in HEAD — first commit with baselines, allow.
		return nil
	}

	var headBaselines Baselines
	if err := json.Unmarshal([]byte(headData), &headBaselines); err != nil {
		return fmt.Errorf("parse HEAD baselines: %w", err)
	}

	// Read staged version.
	stagedData, err := RunOutputIn(root, "git", "show", ":"+baselinesFile)
	if err != nil {
		return fmt.Errorf("read staged baselines: %w", err)
	}

	var stagedBaselines Baselines
	if err := json.Unmarshal([]byte(stagedData), &stagedBaselines); err != nil {
		return fmt.Errorf("parse staged baselines: %w", err)
	}

	// Check each platform/metric in HEAD against staged.
	var regressions []string
	for platform, headMetrics := range headBaselines {
		stagedMetrics, ok := stagedBaselines[platform]
		if !ok {
			regressions = append(regressions, fmt.Sprintf("  %s: platform removed entirely", platform))
			continue
		}
		for metric, headBl := range headMetrics {
			// Only validate Enforced entries (Direction != "" and Value != nil in HEAD).
			if headBl.Direction == "" || headBl.Value == nil {
				continue
			}
			stagedBl, ok := stagedMetrics[metric]
			if !ok {
				regressions = append(regressions, fmt.Sprintf("  %s/%s: metric removed", platform, metric))
				continue
			}
			stagedVal := float64(0)
			if stagedBl.Value != nil {
				stagedVal = *stagedBl.Value
			}
			if !checkRatchet(headBl.Direction, *headBl.Value, stagedVal) {
				regressions = append(regressions, fmt.Sprintf("  %s/%s: %v → %v (%s)",
					platform, metric, *headBl.Value, stagedVal, ratchetVerb(headBl.Direction)))
			}
		}
	}

	if len(regressions) > 0 {
		return fmt.Errorf("baselines.json regression detected:\n%s", strings.Join(regressions, "\n"))
	}
	return nil
}
