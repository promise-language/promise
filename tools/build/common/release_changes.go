package common

import (
	"flag"
	"fmt"
	"io"
	"os"
)

// release_changes.go implements `bin/release changes [--commit-hash <sha>]` — a
// read-only preview of the non-merge commit subjects that would go into the next
// stable release's notes. Output is pipeable for AI summarizers (one subject per
// line, preceded by a count header line). No tagging, no network beyond
// `git fetch --tags` to ensure epoch tags are current.

// runReleaseChanges is the CLI entry point for `bin/release changes`.
func runReleaseChanges(root string, args []string) error {
	fs := flag.NewFlagSet("changes", flag.ContinueOnError)
	commitHash := fs.String("commit-hash", "", "upper bound: must be HEAD or ancestor of HEAD (default: HEAD)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	git := defaultCutGit(root)
	if err := git.Fetch(); err != nil {
		return fmt.Errorf("changes: git fetch: %w", err)
	}
	return releaseChanges(git, os.Stdout, *commitHash)
}

// releaseChanges is the testable core: resolves the upper bound, finds the last
// stable epoch tag, and prints the commit subjects between them to out.
func releaseChanges(git cutGit, out io.Writer, commitHash string) error {
	upper, err := resolveChangesUpper(git, commitHash)
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
// commitHash is empty it returns HEAD; otherwise it validates that commitHash is
// reachable from HEAD (mirrors the `ci --commit-hash` ancestry check).
func resolveChangesUpper(git cutGit, commitHash string) (string, error) {
	head, err := git.HeadSHA()
	if err != nil {
		return "", fmt.Errorf("changes: resolve HEAD: %w", err)
	}
	if commitHash == "" {
		return head, nil
	}
	anc, err := git.IsAncestor(commitHash, head)
	if err != nil {
		return "", fmt.Errorf("changes: ancestry check: %w", err)
	}
	if !anc {
		return "", fmt.Errorf("changes: commit %s is not reachable from HEAD (%s): --commit-hash must be HEAD or an ancestor",
			short(commitHash), short(head))
	}
	return commitHash, nil
}
