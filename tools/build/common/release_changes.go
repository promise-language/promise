package common

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// release_changes.go implements `bin/release changes [--commit <commit>]` — a
// read-only preview of the non-merge commit subjects that would go into the next
// stable release's notes. Output is pipeable for AI summarizers (one subject per
// line, preceded by a count header line). No tagging, no network beyond
// `git fetch --tags` to ensure epoch tags are current.

// runReleaseChanges is the CLI entry point for `bin/release changes`.
func runReleaseChanges(root string, args []string) error {
	fs := flag.NewFlagSet("changes", flag.ContinueOnError)
	commit := fs.String("commit", "", "upper bound: must be HEAD or ancestor of HEAD (default: HEAD)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	git := defaultCutGit(root)
	if err := git.Fetch(); err != nil {
		return fmt.Errorf("changes: git fetch: %w", err)
	}
	return releaseChanges(git, os.Stdout, *commit)
}

// releaseChanges is the testable core: resolves the upper bound, finds the last
// stable epoch tag, and prints the commit subjects between them to out.
func releaseChanges(git cutGit, out io.Writer, commit string) error {
	upper, err := resolveChangesUpper(git, commit)
	if err != nil {
		return err
	}
	last, haveLast, err := highestReleasedEpoch(git)
	if err != nil {
		return err
	}
	var fromRef string
	if haveLast {
		fromRef = "epoch-" + last.String()
	}
	subjects, err := git.LogSubjects(fromRef, upper)
	if err != nil {
		return fmt.Errorf("changes: git log: %w", err)
	}
	if haveLast {
		fmt.Fprintf(out, "%d commits since %s\n", len(subjects), fromRef)
	} else {
		fmt.Fprintf(out, "%d commits (no prior stable epoch)\n", len(subjects))
	}
	for _, s := range subjects {
		fmt.Fprintln(out, s)
	}
	return nil
}

// resolveChangesUpper returns the upper SHA bound for the changes range. If
// commit is empty it returns HEAD; otherwise the commit-ish is peeled to a
// concrete SHA (so a branch/tag/`HEAD~2` works, same as `ci --commit` and
// `cut --commit`) and validated as reachable from HEAD.
func resolveChangesUpper(git cutGit, commit string) (string, error) {
	head, err := git.HeadSHA()
	if err != nil {
		return "", fmt.Errorf("changes: resolve HEAD: %w", err)
	}
	if commit == "" {
		return head, nil
	}
	sha, err := git.ResolveSHA(commit)
	if err != nil {
		return "", fmt.Errorf("changes: resolve --commit %q: %w", commit, err)
	}
	sha = strings.TrimSpace(sha)
	anc, err := git.IsAncestor(sha, head)
	if err != nil {
		return "", fmt.Errorf("changes: ancestry check: %w", err)
	}
	if !anc {
		return "", fmt.Errorf("changes: commit %s is not reachable from HEAD (%s): --commit must be HEAD or an ancestor",
			short(sha), short(head))
	}
	return sha, nil
}
