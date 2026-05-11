package common

import (
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
// include compiled binaries.
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
	return nil
}
