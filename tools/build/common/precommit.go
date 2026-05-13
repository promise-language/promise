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
	out, err := RunOutputIn(root, "git", "diff", "--cached", "--name-only")
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

	return nil
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
