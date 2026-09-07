package common

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// blockedFiles lists compiled binaries that must never be committed. These are
// named explicitly only so they get a clearer message than the general
// binary-content gate below — they are not an exemption list.
var blockedFiles = []string{
	"compiler/promise",
	"compiler/promise.exe",
	"bin/promise",
	"bin/promise.exe",
}

// maxCommittedFileSize is the largest staged blob the pre-commit hook admits.
// Files over this must be generated at build time from tracked sources, not
// committed. (T1620)
const maxCommittedFileSize = 1 << 20 // 1 MiB

// binaryScanLimit is git's buffer_is_binary window: a blob with a NUL byte in
// its first 8 KB is treated as binary and cannot be reviewed. (T1620)
const binaryScanLimit = 8192

// noreplyDomain is the only email domain permitted for commit identities. Using
// a GitHub noreply address keeps a personal email out of the public history.
const noreplyDomain = "@users.noreply.github.com"

// checkNoreplyIdentity refuses the commit unless both the author and committer
// emails are GitHub noreply addresses. It reads the identities via 'git var',
// which resolves them exactly as the impending commit will — honoring
// 'git commit --author=...', GIT_AUTHOR_EMAIL / GIT_COMMITTER_EMAIL env vars,
// and user.email config alike — so the check matches what would actually be
// recorded.
func checkNoreplyIdentity(root string) error {
	roles := []struct{ label, gitVar string }{
		{"author", "GIT_AUTHOR_IDENT"},
		{"committer", "GIT_COMMITTER_IDENT"},
	}
	for _, r := range roles {
		ident, err := RunOutputIn(root, "git", "var", r.gitVar)
		if err != nil {
			return fmt.Errorf("reading %s identity: %w", r.label, err)
		}
		email := identEmail(ident)
		if !strings.HasSuffix(strings.ToLower(email), noreplyDomain) {
			return fmt.Errorf("%s email %q is not a %s address — set one with:\n  git config user.email \"<id>+<user>%s\"",
				r.label, email, noreplyDomain, noreplyDomain)
		}
	}
	return nil
}

// identEmail extracts the address from a git ident string of the form
// "Name <email> <timestamp> <tz>". It returns "" if no <…> field is present.
func identEmail(ident string) string {
	open := strings.IndexByte(ident, '<')
	closeIdx := strings.IndexByte(ident, '>')
	if open < 0 || closeIdx < open {
		return ""
	}
	return ident[open+1 : closeIdx]
}

// isTestPrFile reports whether a repo-relative path is a Promise test file —
// either any .pr file under tests/ or any file whose name ends in _test.pr
// (module tests under modules/).
func isTestPrFile(rel string) bool {
	rel = filepath.ToSlash(rel)
	if strings.HasSuffix(rel, ".pr") && strings.HasPrefix(rel, "tests/") {
		return true
	}
	return strings.HasSuffix(rel, "_test.pr")
}

// sleepOKMarker annotates a single sleep() call as legitimate. It is the only
// escape hatch from the T1632 guard, and it is deliberately per-line rather than
// per-file: a file-level allowlist re-permits every future sleep in a file that
// earned its entry for one call, which is exactly how the pattern crept back in
// the first place. The marker must carry a reason.
const sleepOKMarker = "// sleep-ok:"

// isIdentByte reports whether b can appear inside a Promise identifier.
func isIdentByte(b byte) bool {
	return b == '_' ||
		(b >= '0' && b <= '9') ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z')
}

// containsSleepCall reports whether code calls sleep(). It requires a word
// boundary before the name, so an identifier that merely ends in "sleep" is not
// a call — `test_sleep()` declares a test, it does not synchronize on one.
func containsSleepCall(code string) bool {
	for i := 0; ; {
		j := strings.Index(code[i:], "sleep(")
		if j < 0 {
			return false
		}
		at := i + j
		if at == 0 || !isIdentByte(code[at-1]) {
			return true
		}
		i = at + len("sleep(")
	}
}

// sleepCallLines returns the 1-indexed line numbers of every sleep() call in
// data that is not annotated with a non-empty `// sleep-ok: <reason>` marker on
// the same line. A sleep( that appears only inside a comment is not a call.
func sleepCallLines(data []byte) []int {
	var lines []int
	for i, line := range strings.Split(string(data), "\n") {
		code, comment := line, ""
		if j := strings.Index(line, "//"); j >= 0 {
			code, comment = line[:j], line[j:]
		}
		if !containsSleepCall(code) {
			continue
		}
		if k := strings.Index(comment, sleepOKMarker); k >= 0 &&
			strings.TrimSpace(comment[k+len(sleepOKMarker):]) != "" {
			continue // annotated with a reason — permitted
		}
		lines = append(lines, i+1)
	}
	return lines
}

// CheckTestSleeps scans all tracked Promise test files and returns an error
// naming every unannotated sleep() call site. It uses git ls-files so it covers
// the full index, not just staged files — a violation committed on a previous
// turn is caught on the next pre-commit invocation.
func CheckTestSleeps(root string) error {
	out, err := RunOutputIn(root, "git", "ls-files", "-z", "*.pr")
	if err != nil {
		return fmt.Errorf("list tracked Promise files: %w", err)
	}

	// git ls-files emits paths in sorted index order, and sleepCallLines returns
	// line numbers in ascending order, so violations come out already ordered.
	var violations []string
	for _, rel := range strings.Split(out, "\x00") {
		if rel == "" {
			continue
		}
		slashRel := filepath.ToSlash(rel)
		if !isTestPrFile(slashRel) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			// Tracked but absent from the worktree (staged deletion). Skip.
			continue
		}
		for _, line := range sleepCallLines(data) {
			violations = append(violations, fmt.Sprintf("  %s:%d", slashRel, line))
		}
	}
	if len(violations) > 0 {
		return fmt.Errorf("test files synchronize on sleep() (T1632 — order concurrent operations "+
			"with a channel, a ready handshake or an awaited task):\n%s\n"+
			"If a call genuinely measures time rather than ordering two concurrent operations, "+
			"annotate that line with `%s <why>`.",
			strings.Join(violations, "\n"), sleepOKMarker)
	}
	return nil
}

// RunPreCommit implements the git pre-commit hook. It rejects commits that
// include compiled binaries and validates baselines.json ratchet direction.
func RunPreCommit(root string) error {
	if err := checkNoreplyIdentity(root); err != nil {
		return err
	}

	// Mechanical documentation checks: dangling relative .md links,
	// docs/index.md coverage, and catalog/doc module coverage. Cheap enough
	// to run unconditionally, and each catches drift that is otherwise only
	// found by a manual sweep (T1675). Deliberately ahead of the staged-file
	// scan, which returns early when nothing is staged.
	if err := CheckDocs(root); err != nil {
		return err
	}

	// Structural anti-regression guard: reject sleep()-as-synchronization in
	// test .pr files (T1615/T1632). Runs unconditionally on the full index,
	// same as CheckDocs, so a violation committed on a prior turn is caught.
	if err := CheckTestSleeps(root); err != nil {
		return err
	}

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

	// General no-binary + size gates: reject any staged blob larger than 1 MB,
	// and any staged blob with binary content unless it is declared binary in
	// .gitattributes. (T1620)
	if err := checkStagedBlobs(root, staged); err != nil {
		return err
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

// checkStagedBlobs enforces two independent gates on every staged blob — the
// index copy (`:path`), i.e. what is actually being committed, not the worktree
// file:
//   - no blob larger than maxCommittedFileSize (a binary can't be reviewed, and
//     large generated artifacts should be built, not committed)
//   - no binary content (NUL in the first 8 KB) unless the path is declared
//     binary in .gitattributes
//
// The two catch different mistakes and neither subsumes the other: a 4 MB
// generated JSON is not binary, and a 2 KB .so stub is not large.
func checkStagedBlobs(root string, staged []string) error {
	for _, f := range staged {
		if f == "" {
			continue
		}
		// Size of the staged blob. A staged deletion (or otherwise absent index
		// entry, e.g. a submodule gitlink) makes `cat-file -s :path` fail —
		// nothing to review, skip. The probe is quiet because that failure is
		// expected and handled here; otherwise git's `fatal:` diagnostic would
		// leak to the terminal on every commit that removes a file. Size is
		// checked first so an oversized blob is rejected before it is read into
		// memory.
		sizeStr, err := RunOutputQuietIn(root, "git", "cat-file", "-s", ":"+f)
		if err != nil {
			continue
		}
		size, err := strconv.Atoi(sizeStr)
		if err != nil {
			return fmt.Errorf("parse staged size of '%s': %w", f, err)
		}
		if size > maxCommittedFileSize {
			return fmt.Errorf("staged file '%s' is %d bytes — over the 1 MB limit; "+
				"generate large artifacts at build time instead of committing them.\n  git reset HEAD %s",
				f, size, f)
		}

		blob, err := RunBytesIn(root, "git", "cat-file", "blob", ":"+f)
		if err != nil {
			return fmt.Errorf("read staged blob '%s': %w", f, err)
		}
		head := blob
		if len(head) > binaryScanLimit {
			head = head[:binaryScanLimit]
		}
		if bytes.IndexByte(head, 0) < 0 {
			continue // text — fine
		}
		declared, err := pathDeclaredBinary(root, f)
		if err != nil {
			return err
		}
		if !declared {
			return fmt.Errorf("staged file '%s' contains binary content (NUL byte in first 8 KB) "+
				"but is not declared binary in .gitattributes — binaries cannot be reviewed. "+
				"Either it should not be committed, or add an explicit .gitattributes entry (e.g. '%s binary').\n  git reset HEAD %s",
				f, f, f)
		}
	}
	return nil
}

// pathDeclaredBinary reports whether .gitattributes marks path as a reviewed
// binary — either `binary` is set, or `text` is unset (`-text`). Both are how
// the repo already declares binary blobs (tests/embed/*.bin uses `-text`; the
// WASM CRT objects use `binary`). It queries git rather than re-implementing
// attribute matching, and reads the working-tree .gitattributes (which is what
// will be committed when an escape-hatch line is staged alongside the binary).
func pathDeclaredBinary(root, path string) (bool, error) {
	out, err := RunOutputIn(root, "git", "check-attr", "-z", "binary", "text", "--", path)
	if err != nil {
		return false, fmt.Errorf("check-attr for '%s': %w", path, err)
	}
	// -z output is a stream of (path, attr, value) NUL-separated triples.
	// Group-of-3 parsing is robust even for a file literally named
	// "binary"/"text" (the path token is skipped).
	fields := strings.Split(out, "\x00")
	for i := 0; i+2 < len(fields); i += 3 {
		switch attr, value := fields[i+1], fields[i+2]; {
		case attr == "binary" && value == "set":
			return true, nil
		case attr == "text" && value == "unset":
			return true, nil
		}
	}
	return false, nil
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
