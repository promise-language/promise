package common

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// initGitRepoWithStager creates a temp git repo with one seed commit and a
// noreply identity, and returns the repo root plus a stage function that writes
// arbitrary bytes at a repo-relative path and `git add`s it. Every sweep in
// structuralChecks reads the index, so `git add` is what puts a fixture in
// scope; staging exact bytes is also what lets a test place content no text
// editor would produce.
func initGitRepoWithStager(t *testing.T) (string, func(rel string, content []byte)) {
	t.Helper()
	root := t.TempDir()

	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=1+test@users.noreply.github.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=1+test@users.noreply.github.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	git("init")
	git("config", "user.name", "test")
	git("config", "user.email", "1+test@users.noreply.github.com")

	os.WriteFile(filepath.Join(root, "seed.txt"), []byte("seed\n"), 0o644)
	git("add", "seed.txt")
	git("commit", "-m", "seed")

	stage := func(rel string, content []byte) {
		t.Helper()
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", rel)
	}

	return root, stage
}

// TestRunBytesIn_PreservesRawBytes locks RunBytesIn's contract: it returns the
// child's stdout byte-for-byte, untrimmed. This is why WorktreeHash (hash.go)
// uses it instead of RunOutputIn, whose TrimSpace would eat the terminator of a
// `git ls-files -z` stream and silently merge the last path into nothing. The
// seed file is committed as "seed\n"; RunOutputIn would strip the trailing
// newline, RunBytesIn must not. Guards against a "simplification" that reroutes
// RunBytesIn through the trimming helper.
func TestRunBytesIn_PreservesRawBytes(t *testing.T) {
	root, _ := initGitRepoWithStager(t)
	raw, err := RunBytesIn(root, "git", "cat-file", "blob", ":seed.txt")
	if err != nil {
		t.Fatalf("RunBytesIn: %v", err)
	}
	if string(raw) != "seed\n" {
		t.Fatalf("RunBytesIn returned %q, want %q (trailing newline must be preserved)", raw, "seed\n")
	}
}

// initBareGitRepo creates an empty temp git repo (no commits) — enough for the
// checks in this package that never look at HEAD, which is every caller of
// 'git ls-files' (it reads the index and the worktree). Contrast initGitRepo in
// version_test.go, which seeds a commit because the provenance tests need a
// SHA.
func initBareGitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	cmd := exec.Command("git", "init")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return root
}

func TestUnformattedGoFiles_DetectsAndSkips(t *testing.T) {
	root := t.TempDir()
	mk := func(rel, content string) {
		full := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(content), 0o644)
	}
	mk("compiler/a.go", "package a\n\nfunc F() {}\n")      // formatted
	mk("compiler/b.go", "package b\nfunc  G(){}\n")        // unformatted
	mk("compiler/vendor/c.go", "package c\nfunc  H(){}\n") // skipped (vendor)
	mk("compiler/notes.txt", "func  not_go(){}\n")         // skipped (not .go)

	got, err := UnformattedGoFiles(root)
	if err != nil {
		t.Fatalf("UnformattedGoFiles: %v", err)
	}
	if len(got) != 1 || got[0] != filepath.Join("compiler", "b.go") {
		t.Fatalf("expected [compiler/b.go], got %v", got)
	}
}

// TestGoFileDirs_ExcludesFlowsWhenAbsent verifies that goFileDirs returns only
// compiler/ and tools/build/ when flows/go.mod is absent (the common case on
// main without the flows branch checked out).
func TestGoFileDirs_ExcludesFlowsWhenAbsent(t *testing.T) {
	root := t.TempDir()
	dirs := goFileDirs(root)
	for _, d := range dirs {
		if filepath.Base(d) == "flows" {
			t.Errorf("goFileDirs: unexpected flows/ entry when flows/go.mod absent: %v", dirs)
		}
	}
	if len(dirs) != 2 {
		t.Errorf("goFileDirs: expected 2 dirs (compiler, tools/build), got %d: %v", len(dirs), dirs)
	}
}

// TestGoFileDirs_IncludesFlowsWhenPresent verifies that goFileDirs appends
// flows/ when flows/go.mod exists — the flows formatting path added by T0743.
func TestGoFileDirs_IncludesFlowsWhenPresent(t *testing.T) {
	root := t.TempDir()
	flowsDir := filepath.Join(root, "flows")
	if err := os.MkdirAll(flowsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(flowsDir, "go.mod"), []byte("module example.com/flows\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirs := goFileDirs(root)
	found := false
	for _, d := range dirs {
		if d == flowsDir {
			found = true
		}
	}
	if !found {
		t.Errorf("goFileDirs: expected flows/ in dirs when flows/go.mod present, got %v", dirs)
	}
	if len(dirs) != 3 {
		t.Errorf("goFileDirs: expected 3 dirs (compiler, tools/build, flows), got %d: %v", len(dirs), dirs)
	}
}

// TestUnformattedGoFiles_IncludesFlowsDir verifies that UnformattedGoFiles
// scans flows/ for unformatted Go files when flows/go.mod is present. This
// exercises the T0743 path where flows code is covered by the format check.
func TestUnformattedGoFiles_IncludesFlowsDir(t *testing.T) {
	root := t.TempDir()
	mk := func(rel, content string) {
		full := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(content), 0o644)
	}
	mk("flows/go.mod", "module example.com/flows\n\ngo 1.21\n")
	mk("flows/do/main.go", "package do\nfunc  BadlyFormatted(){}\n") // unformatted

	got, err := UnformattedGoFiles(root)
	if err != nil {
		t.Fatalf("UnformattedGoFiles: %v", err)
	}
	if len(got) != 1 || got[0] != filepath.Join("flows", "do", "main.go") {
		t.Fatalf("expected [flows/do/main.go], got %v", got)
	}
}

func TestUnformattedPromiseFiles_SkipsWithoutCompiler(t *testing.T) {
	// No bin/promise in a temp repo → can't check Promise, must skip (not error).
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "modules"), 0o755)
	os.WriteFile(filepath.Join(root, "modules", "x.pr"), []byte("main(){}\n"), 0o644)
	got, err := UnformattedPromiseFiles(root)
	if err != nil {
		t.Fatalf("expected nil error when bin/promise absent, got: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil (skipped), got %v", got)
	}
}

// --- sleep() guard tests (T1615/T1632) ---

func TestIsTestPrFile_MatchesTestsDir(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"tests/foo/bar.pr", true},
		{"tests/e2e/basics.pr", true},
		{"tests/concurrency/goroutine_fire_and_forget.pr", true},
	}
	for _, c := range cases {
		if got := isTestPrFile(c.path); got != c.want {
			t.Errorf("isTestPrFile(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestIsTestPrFile_MatchesModuleTestFile(t *testing.T) {
	cases := []string{
		"modules/http/http_test.pr",
		"modules/net/net_test.pr",
		"modules/x/x_test.pr",
	}
	for _, p := range cases {
		if !isTestPrFile(p) {
			t.Errorf("isTestPrFile(%q) = false, want true", p)
		}
	}
}

func TestIsTestPrFile_IgnoresNonTestPr(t *testing.T) {
	cases := []string{
		"modules/x/x.pr",
		"modules/http/http.pr",
		"compiler/x.go",
		"modules/std/string.pr",
	}
	for _, p := range cases {
		if isTestPrFile(p) {
			t.Errorf("isTestPrFile(%q) = true, want false", p)
		}
	}
}

func TestSleepCallLines_DetectsCall(t *testing.T) {
	data := []byte("  sleep(Duration.from_millis(50));\n")
	if got := sleepCallLines(data); len(got) != 1 || got[0] != 1 {
		t.Errorf("sleepCallLines = %v, want [1]", got)
	}
}

func TestSleepCallLines_IgnoresLineComment(t *testing.T) {
	data := []byte("// sleep() only made that *likely*; the receive makes it ordered.\n")
	if got := sleepCallLines(data); len(got) != 0 {
		t.Errorf("sleepCallLines = %v, want none for sleep() in a comment", got)
	}
}

func TestSleepCallLines_DetectsCallAfterCode(t *testing.T) {
	// sleep() after a // comment on the same line → not a real call.
	commentOnly := []byte("x := 1; // not sleep(\n")
	if got := sleepCallLines(commentOnly); len(got) != 0 {
		t.Errorf("sleepCallLines = %v, want none when sleep( appears only after //", got)
	}
	// sleep() before any comment → real call.
	realCall := []byte("x := 1; sleep(ms);\n")
	if got := sleepCallLines(realCall); len(got) != 1 {
		t.Errorf("sleepCallLines = %v, want one call before the comment", got)
	}
}

// A `// sleep-ok: <reason>` marker on the same line permits that one call.
func TestSleepCallLines_AllowsMarkerWithReason(t *testing.T) {
	data := []byte("  sleep(d); // sleep-ok: the swept delay is the subject under test\n")
	if got := sleepCallLines(data); len(got) != 0 {
		t.Errorf("sleepCallLines = %v, want none for an annotated call", got)
	}
}

// The marker must carry a reason — a bare marker is not an escape hatch.
func TestSleepCallLines_RejectsMarkerWithoutReason(t *testing.T) {
	for _, line := range []string{
		"  sleep(d); // sleep-ok:\n",
		"  sleep(d); // sleep-ok:   \n",
	} {
		if got := sleepCallLines([]byte(line)); len(got) != 1 {
			t.Errorf("sleepCallLines(%q) = %v, want the call reported (no reason given)", line, got)
		}
	}
}

// A marker on a different line does not cover the call.
func TestSleepCallLines_RejectsMarkerOnAnotherLine(t *testing.T) {
	data := []byte("// sleep-ok: this reason is on the wrong line\n  sleep(d);\n")
	got := sleepCallLines(data)
	if len(got) != 1 || got[0] != 2 {
		t.Errorf("sleepCallLines = %v, want [2] — the marker does not reach the next line", got)
	}
}

// Every unannotated site in a multi-site file is reported, not just the first.
func TestSleepCallLines_ReportsEverySite(t *testing.T) {
	data := []byte("sleep(a);\nx := 1;\nsleep(b); // sleep-ok: measured\nsleep(c);\n")
	got := sleepCallLines(data)
	if len(got) != 2 || got[0] != 1 || got[1] != 4 {
		t.Errorf("sleepCallLines = %v, want [1 4]", got)
	}
}

// TestCheckTestSleeps_RejectsViolation creates a git repo with a test .pr file
// that calls sleep() and asserts CheckTestSleeps returns an error naming the path.
func TestCheckTestSleeps_RejectsViolation(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	stage("tests/foo/bad_test.pr", []byte("test_x() `test { sleep(Duration.from_millis(10)); }\n"))
	err := CheckTestSleeps(root)
	if err == nil {
		t.Fatal("expected error for sleep() in test file, got nil")
	}
	if !strings.Contains(err.Error(), "tests/foo/bad_test.pr:1") {
		t.Errorf("error should name the violating path AND line, got: %v", err)
	}
}

// TestCheckTestSleeps_AllowsAnnotatedSite verifies that a sleep() carrying a
// `// sleep-ok: <reason>` marker is not reported. The guard is site-granular, so
// nothing about the path matters — only the annotation on that line.
func TestCheckTestSleeps_AllowsAnnotatedSite(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	stage("tests/std/time_test.pr",
		[]byte("test_t() `test { sleep(Duration.from_millis(15)); } // sleep-ok: sleep() is the subject under test\n"))
	if err := CheckTestSleeps(root); err != nil {
		t.Fatalf("expected no error for an annotated sleep site, got: %v", err)
	}
}

// The same path is NOT privileged when the annotation is missing — the previous
// file-level allowlist would have permitted this.
func TestCheckTestSleeps_RejectsUnannotatedSiteInAnyFile(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	stage("tests/std/time_test.pr", []byte("test_t() `test { sleep(Duration.from_millis(15)); }\n"))
	err := CheckTestSleeps(root)
	if err == nil {
		t.Fatal("expected error for an unannotated sleep, got nil")
	}
	if !strings.Contains(err.Error(), "tests/std/time_test.pr:1") {
		t.Errorf("error should name the site, got: %v", err)
	}
}

// TestCheckTestSleeps_AllowsCommentOnly verifies that sleep() appearing only
// inside a line comment is not flagged.
func TestCheckTestSleeps_AllowsCommentOnly(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	stage("tests/foo/doc_test.pr",
		[]byte("// sleep() is not needed here — use a channel instead.\ntest_x() `test { assert(1 == 1); }\n"))
	if err := CheckTestSleeps(root); err != nil {
		t.Fatalf("expected no error for comment-only sleep reference, got: %v", err)
	}
}

// TestCheckTestSleeps_IgnoresNonTestFile verifies that a non-test .pr file (no
// _test suffix, not under tests/) with a sleep() call is not flagged.
func TestCheckTestSleeps_IgnoresNonTestFile(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	stage("modules/http/http.pr", []byte("  sleep(Duration.from_millis(10));\n"))
	if err := CheckTestSleeps(root); err != nil {
		t.Fatalf("expected no error for sleep() in non-test file, got: %v", err)
	}
}

// TestCheckTestSleeps_SkipsTrackedButAbsent verifies that a test .pr file
// tracked in the git index but absent from the worktree (deleted without
// staging) is skipped gracefully — no error is returned. This covers the
// os.ReadFile error path inside CheckTestSleeps, which is documented as the
// staged-deletion case.
func TestCheckTestSleeps_SkipsTrackedButAbsent(t *testing.T) {
	root, stage := initGitRepoWithStager(t)

	// Stage a test file with sleep() so it's in the index.
	stage("tests/foo/absent_test.pr", []byte("test_x() `test { sleep(Duration.from_millis(10)); }\n"))

	// Commit it so it appears in git ls-files even after worktree deletion.
	git := exec.Command("git", "commit", "-m", "add test")
	git.Dir = root
	git.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=1+test@users.noreply.github.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=1+test@users.noreply.github.com",
	)
	if out, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}

	// Delete from worktree without staging — file stays in git ls-files output.
	if err := os.Remove(filepath.Join(root, "tests", "foo", "absent_test.pr")); err != nil {
		t.Fatal(err)
	}

	// CheckTestSleeps must not return an error — the absent file is skipped.
	if err := CheckTestSleeps(root); err != nil {
		t.Fatalf("expected no error for tracked-but-absent test file, got: %v", err)
	}
}

// An identifier that merely ends in "sleep" is not a call — `test_sleep()` is a
// test declaration. This false positive was previously masked by the file-level
// allowlist that covered tests/std/time_test.pr.
func TestSleepCallLines_IgnoresIdentifierEndingInSleep(t *testing.T) {
	for _, line := range []string{
		"test_sleep() `test(exclude: wasm) {\n",
		"_drain_after_sleep(int fd) int {\n",
	} {
		if got := sleepCallLines([]byte(line)); len(got) != 0 {
			t.Errorf("sleepCallLines(%q) = %v, want none — not a sleep() call", line, got)
		}
	}
}

// A qualified or bare call is still a call.
func TestSleepCallLines_DetectsQualifiedAndBareCalls(t *testing.T) {
	for _, line := range []string{
		"  sleep(d);\n",
		"  time.sleep(d);\n",
		"  x := 1; sleep(d);\n",
	} {
		if got := sleepCallLines([]byte(line)); len(got) != 1 {
			t.Errorf("sleepCallLines(%q) = %v, want one call", line, got)
		}
	}
}

// A non-boundary match must not stop the scan: an identifier ending in "sleep"
// earlier on the line hides a real call after it. This is the loop-advance in
// containsSleepCall, and the shape that actually occurs — a helper named
// _drain_after_sleep calling sleep() on its own line.
func TestSleepCallLines_DetectsCallAfterNonBoundaryMatch(t *testing.T) {
	for _, line := range []string{
		"  no_sleep(); sleep(d);\n",
		"  _drain_after_sleep(fd); sleep(d);\n",
	} {
		if got := sleepCallLines([]byte(line)); len(got) != 1 || got[0] != 1 {
			t.Errorf("sleepCallLines(%q) = %v, want [1] — the second match is a real call", line, got)
		}
	}
}

// The guard reports call *sites* by line, so two calls on one line are one
// violation. The reported line still points the reader at both.
func TestSleepCallLines_ReportsOneEntryPerLine(t *testing.T) {
	data := []byte("x := 1;\nsleep(a); sleep(b);\n")
	got := sleepCallLines(data)
	if len(got) != 1 || got[0] != 2 {
		t.Errorf("sleepCallLines = %v, want [2] — two calls on one line are one entry", got)
	}
}

// CheckTestSleeps surfaces a git failure rather than silently reporting a clean
// tree: with no index to scan there are no files to check, and treating that as
// "no violations" would make the guard vacuous wherever it misfires.
//
// The root is a path that does not exist, not merely one outside a repository:
// TMPDIR is sometimes inside the checkout (verify points it at .promise-home/),
// and git would then walk up and find the real repo, quietly making this pass
// for the wrong reason. A missing directory fails the same way from anywhere.
func TestCheckTestSleeps_ErrorsWhenGitCannotList(t *testing.T) {
	err := CheckTestSleeps(filepath.Join(t.TempDir(), "no-such-dir"))
	if err == nil {
		t.Fatal("expected an error when git cannot list the index, got nil")
	}
	if !strings.Contains(err.Error(), "list tracked *.pr files") {
		t.Errorf("error should name the failing step, got: %v", err)
	}
}

// --- T1963: scratch paths must be unique per process ---

func TestTempDirLines_DetectsFixedPath(t *testing.T) {
	data := []byte("  string p = os.temp_dir + \"/pr_iot_cer\";\n")
	if got := tempDirLines(data); len(got) != 1 || got[0] != 1 {
		t.Errorf("tempDirLines = %v, want [1]", got)
	}
}

// The hoisted form — a bare assignment with no concatenation — is the shape a
// narrower `temp_dir + "…"` rule would miss. It is exactly as shared.
func TestTempDirLines_DetectsHoistedAssignment(t *testing.T) {
	data := []byte("  string d = os.temp_dir;\n")
	if got := tempDirLines(data); len(got) != 1 || got[0] != 1 {
		t.Errorf("tempDirLines = %v, want [1] for a hoisted temp_dir", got)
	}
}

// A path that names the pid is per-process by construction — the point of the
// rule — so it needs no marker.
func TestTempDirLines_AllowsProcessIDOnSameLine(t *testing.T) {
	data := []byte("_scratch(string n) string => os.temp_dir + \"/pr_\" + os.process_id.to_string() + n;\n")
	if got := tempDirLines(data); len(got) != 0 {
		t.Errorf("tempDirLines = %v, want none when process_id is on the line", got)
	}
}

func TestTempDirLines_AllowsMarkerWithReason(t *testing.T) {
	data := []byte("  string tmp = temp_dir; // temp-dir-ok: temp_dir is the subject under test\n")
	if got := tempDirLines(data); len(got) != 0 {
		t.Errorf("tempDirLines = %v, want none for an annotated site", got)
	}
}

func TestTempDirLines_RejectsMarkerWithoutReason(t *testing.T) {
	data := []byte("  string tmp = temp_dir; // temp-dir-ok:\n")
	if got := tempDirLines(data); len(got) != 1 || got[0] != 1 {
		t.Errorf("tempDirLines = %v, want [1] for a marker with no reason", got)
	}
}

// The marker is per-line, like sleep-ok: one earned exemption must not license
// the next line in the same file.
func TestTempDirLines_RejectsMarkerOnAnotherLine(t *testing.T) {
	data := []byte("  // temp-dir-ok: explained above\n  string d = os.temp_dir;\n")
	if got := tempDirLines(data); len(got) != 1 || got[0] != 2 {
		t.Errorf("tempDirLines = %v, want [2] — a marker on a previous line does not carry", got)
	}
}

func TestTempDirLines_IgnoresComment(t *testing.T) {
	data := []byte("// Use os.temp_dir so the test is portable across platforms.\n")
	if got := tempDirLines(data); len(got) != 0 {
		t.Errorf("tempDirLines = %v, want none for temp_dir in a comment", got)
	}
}

// Literal contents are stripped, so an assertion message or a snapshot's
// expected output mentioning temp_dir does not read as a use.
func TestTempDirLines_IgnoresStringLiteral(t *testing.T) {
	data := []byte("  assert(ok, \"temp_dir should be non-empty\");\n")
	if got := tempDirLines(data); len(got) != 0 {
		t.Errorf("tempDirLines = %v, want none for temp_dir inside a string", got)
	}
}

func TestTempDirLines_IgnoresTripleQuotedBlock(t *testing.T) {
	data := []byte("main() `test(expected: \"\"\"wrote to temp_dir\nread back\"\"\") {\n  assert(1 == 1);\n}\n")
	if got := tempDirLines(data); len(got) != 0 {
		t.Errorf("tempDirLines = %v, want none for temp_dir inside a \"\"\" block", got)
	}
}

// An interpolation is code, not literal text — `"{os.temp_dir}/pr_x"` is as
// idiomatic a spelling of the shared-path bug as the concatenated form, so
// stripping the whole literal would leave the guard blind to it.
func TestTempDirLines_DetectsInterpolatedPath(t *testing.T) {
	data := []byte("  string p = \"{os.temp_dir}/pr_iot_cer\";\n")
	if got := tempDirLines(data); len(got) != 1 || got[0] != 1 {
		t.Errorf("tempDirLines = %v, want [1] for an interpolated temp_dir", got)
	}
}

// …and the pid exemption has to be visible through interpolation too, or the
// fixed form and the correct form would both be flagged.
func TestTempDirLines_AllowsInterpolatedProcessID(t *testing.T) {
	data := []byte("  string p = \"{os.temp_dir}/pr_{os.process_id}/x\";\n")
	if got := tempDirLines(data); len(got) != 0 {
		t.Errorf("tempDirLines = %v, want none when the pid is interpolated too", got)
	}
}

// Two adjacent interpolations must not fuse into one identifier: "{a}{b}" is
// not a use of `ab`. The halves of temp_dir are the case that would matter.
func TestTempDirLines_AdjacentInterpolationsDoNotFuse(t *testing.T) {
	data := []byte("  string s = \"{temp}{_dir}\";\n")
	if got := tempDirLines(data); len(got) != 0 {
		t.Errorf("tempDirLines = %v, want none — adjacent interpolations are separate expressions", got)
	}
}

// A brace inside a nested string literal belongs to that literal, so it must
// not end the interpolation early and spill the rest of the line into code.
func TestTempDirLines_NestedStringInInterpolation(t *testing.T) {
	data := []byte("  print_line(\"{fmt(\\\"}\\\")} temp_dir\");\n")
	if got := tempDirLines(data); len(got) != 0 {
		t.Errorf("tempDirLines = %v, want none — text after the interpolation is still literal", got)
	}
}

// Raw strings do not interpolate (RAW_STRING in PromiseLexer.g4), so a brace in
// one is literal text.
func TestTempDirLines_IgnoresRawString(t *testing.T) {
	data := []byte("  string s = r\"{os.temp_dir}\";\n")
	if got := tempDirLines(data); len(got) != 0 {
		t.Errorf("tempDirLines = %v, want none — a raw string does not interpolate", got)
	}
}

// Identifier boundaries are required on both sides: a declaration named after
// temp_dir is not a use of it.
func TestTempDirLines_IgnoresIdentifierContainingTempDir(t *testing.T) {
	data := []byte("test_temp_dir_nonempty() `test {\n_temp_dir_lookup() string {\n")
	if got := tempDirLines(data); len(got) != 0 {
		t.Errorf("tempDirLines = %v, want none for identifiers merely containing temp_dir", got)
	}
}

func TestTempDirLines_ReportsEverySite(t *testing.T) {
	data := []byte("a := os.temp_dir;\nb := 1;\nc := os.temp_dir + \"/x\";\n")
	got := tempDirLines(data)
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Errorf("tempDirLines = %v, want [1 3]", got)
	}
}

func TestCheckTestTempPaths_RejectsViolation(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	stage("tests/foo/bad_test.pr", []byte("test_x() `test { io.File.remove(os.temp_dir + \"/fixed\")?!; }\n"))
	err := CheckTestTempPaths(root)
	if err == nil {
		t.Fatal("expected error for a fixed temp path in a test file, got nil")
	}
	if !strings.Contains(err.Error(), "tests/foo/bad_test.pr:1") {
		t.Errorf("error should name the violating path AND line, got: %v", err)
	}
}

func TestCheckTestTempPaths_AllowsPerProcessPath(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	stage("modules/io/io_test.pr",
		[]byte("_scratch(string n) string => os.temp_dir + \"/pr_\" + os.process_id.to_string() + n;\n"))
	if err := CheckTestTempPaths(root); err != nil {
		t.Fatalf("expected no error for a per-process path, got: %v", err)
	}
}

func TestCheckTestTempPaths_AllowsAnnotatedSite(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	stage("modules/os/os_test.pr",
		[]byte("test_t() `test { string tmp = temp_dir; } // temp-dir-ok: the getter is the subject\n"))
	if err := CheckTestTempPaths(root); err != nil {
		t.Fatalf("expected no error for an annotated site, got: %v", err)
	}
}

// A non-test .pr file is out of scope: modules/os/os.pr implements temp_dir.
func TestCheckTestTempPaths_IgnoresNonTestFile(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	stage("modules/os/os.pr", []byte("get temp_dir string `public { return _temp_dir_lookup(); }\n"))
	if err := CheckTestTempPaths(root); err != nil {
		t.Fatalf("expected no error for temp_dir in a non-test file, got: %v", err)
	}
}

// Examples run as part of the suite, so a fixed path there collides exactly as
// one in a test file does.
func TestCheckTestTempPaths_ScansExamples(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	stage("examples/08_modules/using_io.pr",
		[]byte("main() { io.File.remove(os.temp_dir + \"/promise_example.txt\")?!; }\n"))
	err := CheckTestTempPaths(root)
	if err == nil {
		t.Fatal("expected error for a fixed temp path in an example, got nil")
	}
	if !strings.Contains(err.Error(), "examples/08_modules/using_io.pr:1") {
		t.Errorf("error should name the example site, got: %v", err)
	}
}

func TestCheckTestTempPaths_SkipsTrackedButAbsent(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	stage("tests/foo/absent_test.pr", []byte("test_x() `test { p := os.temp_dir + \"/x\"; }\n"))

	git := exec.Command("git", "commit", "-m", "add test")
	git.Dir = root
	git.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=1+test@users.noreply.github.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=1+test@users.noreply.github.com",
	)
	if out, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	if err := os.Remove(filepath.Join(root, "tests", "foo", "absent_test.pr")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := CheckTestTempPaths(root); err != nil {
		t.Fatalf("expected no error for a tracked-but-absent file, got: %v", err)
	}
}

// Same reasoning as TestCheckTestSleeps_ErrorsWhenGitCannotList: the guard must
// surface a git failure rather than report a clean tree, and the root has to be
// a path that does not exist rather than merely one outside a repository —
// verify points TMPDIR at .promise-home/ inside the checkout, so git would walk
// up, find the real repo, and make a t.TempDir() root pass for the wrong reason.
func TestCheckTestTempPaths_ErrorsWhenGitCannotList(t *testing.T) {
	err := CheckTestTempPaths(filepath.Join(t.TempDir(), "no-such-dir"))
	if err == nil {
		t.Fatal("expected an error when git cannot list the index, got nil")
	}
	if !strings.Contains(err.Error(), "list tracked *.pr files") {
		t.Errorf("error should name the failing step, got: %v", err)
	}
}

// A nested string literal inside an interpolation is spelled with plain quotes
// (INTERP0_STR in PromiseLexer.g4 pushes INTERP_STR on a bare '"'), so its
// braces must not end the interpolation early. If they did, the scanner would
// fall back into code mode mid-literal and read the rest of the string as code.
func TestTempDirLines_NestedPlainQuotedStringInInterpolation(t *testing.T) {
	data := []byte("  print_line(\"{join(v, \"}\")} temp_dir\");\n")
	if got := tempDirLines(data); len(got) != 0 {
		t.Errorf("tempDirLines = %v, want none — text after the nested string is still literal", got)
	}
}

// …and the converse: a genuine use in an interpolation that also contains a
// nested string is still a use.
func TestTempDirLines_DetectsUseAlongsideNestedString(t *testing.T) {
	data := []byte("  string p = \"{os.temp_dir + join(v, \"}\")}\";\n")
	if got := tempDirLines(data); len(got) != 1 || got[0] != 1 {
		t.Errorf("tempDirLines = %v, want [1] — temp_dir is code here, not literal text", got)
	}
}

// A nested `{…}` inside an interpolation (INTERP0_LBRACE → INTERPN) raises the
// brace depth, so the '}' that closes it must not close the interpolation. If
// depth were not tracked, everything after that '}' would be misread as literal
// text and the temp_dir following it would be missed.
func TestTempDirLines_DetectsUseAfterNestedBraces(t *testing.T) {
	data := []byte("  string p = \"{f({1}) + os.temp_dir}\";\n")
	if got := tempDirLines(data); len(got) != 1 || got[0] != 1 {
		t.Errorf("tempDirLines = %v, want [1] — the inner '}' closes the nested brace, not the interpolation", got)
	}
}

// The pid exemption must survive the same nesting, or the correct spelling would
// be flagged alongside the broken one.
func TestTempDirLines_AllowsProcessIDInsideNestedBraces(t *testing.T) {
	data := []byte("  string p = \"{f({os.process_id}) + os.temp_dir}\";\n")
	if got := tempDirLines(data); len(got) != 0 {
		t.Errorf("tempDirLines = %v, want none — the pid is visible through the nested braces", got)
	}
}

// A char literal holding a double quote (CHAR_LITERAL in PromiseLexer.g4) must
// not be read as a string opener. If it were, the quote parity would invert and
// the *contents* of the next real string would be exposed as code — here that
// would report a temp_dir that is only a word in a message.
func TestTempDirLines_CharLiteralQuoteDoesNotFlipStringParity(t *testing.T) {
	data := []byte("  bool q = c == '\"' && msg == \"temp_dir\";\n")
	if got := tempDirLines(data); len(got) != 0 {
		t.Errorf("tempDirLines = %v, want none — the char literal must not open a string", got)
	}
}

// The `\'` escape inside a char literal must be consumed, or the literal appears
// to close early and the unmatched quote swallows the rest of the line — hiding
// the genuine use that follows it.
func TestTempDirLines_EscapedQuoteCharLiteralDoesNotSwallowLine(t *testing.T) {
	data := []byte("  bool q = c == '\\'' && p == os.temp_dir;\n")
	if got := tempDirLines(data); len(got) != 1 || got[0] != 1 {
		t.Errorf("tempDirLines = %v, want [1] — the escaped quote must not end the char literal", got)
	}
}

// `\{` is an escape (STRING_ESCAPE in PromiseLexer.g4), not an interpolation, so
// what follows it is literal text rather than code.
func TestTempDirLines_IgnoresEscapedBrace(t *testing.T) {
	data := []byte("  print_line(\"a \\{os.temp_dir} b\");\n")
	if got := tempDirLines(data); len(got) != 0 {
		t.Errorf("tempDirLines = %v, want none — \\{ escapes the brace, so this is literal text", got)
	}
}

// An unterminated raw string must terminate the scan of its own line without
// leaking state into the next one — raw strings, unlike \"\"\" blocks, never span
// lines, so the following line is still scanned as code.
func TestTempDirLines_UnterminatedRawStringDoesNotSwallowNextLine(t *testing.T) {
	data := []byte("  string s = r\"{os.temp_dir}\n  string p = os.temp_dir + \"/x\";\n")
	if got := tempDirLines(data); len(got) != 1 || got[0] != 2 {
		t.Errorf("tempDirLines = %v, want [2] — the raw string ends with its line", got)
	}
}

// Code following the close of a \"\"\" block on the same line is still code. The
// existing block test closes at end of line, so it does not pin this down.
func TestTempDirLines_ScansCodeAfterBlockStringCloses(t *testing.T) {
	data := []byte("main() `test(expected: \"\"\"x\"\"\") { string p = os.temp_dir + \"/y\"; }\n")
	if got := tempDirLines(data); len(got) != 1 || got[0] != 1 {
		t.Errorf("tempDirLines = %v, want [1] — scanning resumes after the block closes", got)
	}
}

// The pid exemption is per-line, like the marker. A file that already defines a
// correct per-process helper must not license a hand-written fixed path later —
// that copy-the-literal-form drift is precisely what the guard exists to catch.
func TestTempDirLines_HelperDoesNotExemptLaterFixedPath(t *testing.T) {
	data := []byte("_scratch(string n) string => os.temp_dir + \"/pr_\" + os.process_id.to_string() + n;\n" +
		"\n" +
		"  string p = os.temp_dir + \"/pr_iot_fixed\";\n")
	if got := tempDirLines(data); len(got) != 1 || got[0] != 3 {
		t.Errorf("tempDirLines = %v, want [3] — a correct helper above does not exempt line 3", got)
	}
}

// isTestPrFile admits any .pr under tests/, not only *_test.pr — and the suite's
// real offenders (tests/concurrency/io_syscall_*.pr) are exactly that shape.
func TestCheckTestTempPaths_ScansNonSuffixedTestsFile(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	stage("tests/concurrency/io_syscall_stress.pr",
		[]byte("test_x() `test { string p = os.temp_dir + \"/pr_sc_fixed\"; }\n"))
	err := CheckTestTempPaths(root)
	if err == nil {
		t.Fatal("expected error for a fixed temp path under tests/, got nil")
	}
	if !strings.Contains(err.Error(), "tests/concurrency/io_syscall_stress.pr:1") {
		t.Errorf("error should name the violating site, got: %v", err)
	}
}

// --- the list itself ---

// TestRunStructuralChecks_ThisTreeIsClean runs every sweep in structuralChecks
// over the real checkout, as named subtests so a failure says which invariant
// broke.
//
// This is the second of the two paths deliberately, not a duplicate of the
// first. bin/verify calling RunStructuralChecks is what fails a run in seconds;
// this is what still fails if that call is ever deleted — which is exactly what
// happened to these checks once already, when the pre-commit trampoline stopped
// naming the tool that ran them and nothing noticed for as long as it took
// someone to read the hook (T2160). Two independent callers is the whole point.
//
// It iterates the list rather than naming four checks, so a sweep added to
// structuralChecks is covered here from the moment it is added.
func TestRunStructuralChecks_ThisTreeIsClean(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	// A guard, not a courtesy: if the relative path ever stops reaching the
	// repository root, every check below scopes itself out and the test passes
	// while asserting nothing.
	if !Exists(filepath.Join(root, ".git")) {
		t.Fatalf("%s must reach the repository root, or this test is a silent no-op", root)
	}
	for _, c := range structuralChecks {
		t.Run(c.name, func(t *testing.T) {
			if err := c.check(root); err != nil {
				t.Fatalf("this checkout does not reconcile:\n%v", err)
			}
		})
	}
}

// RunStructuralChecks reports every failing sweep, not just the first. A run
// that stopped early would turn one fix-and-rerun cycle into four, and the
// caller printing the result has no way to recover what it did not run.
func TestRunStructuralChecks_ReportsEveryFailingSweep(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	stage("tests/a_test.pr", []byte("test_x() `test { sleep(10ms); }\n"))
	stage("tests/b_test.pr", []byte("test_y() `test { string p = os.temp_dir + \"/fixed\"; }\n"))

	err := RunStructuralChecks(root)
	if err == nil {
		t.Fatal("expected an error when two sweeps fail, got nil")
	}
	for _, want := range []string{"test-sleeps:", "test-temp-paths:", "tests/a_test.pr:1", "tests/b_test.pr:1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should contain %q, got:\n%v", want, err)
		}
	}
}

// A clean tree is not an error. The bare repo has no .pr files at all, so every
// sweep scopes itself out and RunStructuralChecks must join nothing into nil —
// errors.Join's contract, pinned here because a caller returning a non-nil
// "no problems" would fail every verify run.
func TestRunStructuralChecks_CleanTreeIsNil(t *testing.T) {
	root, _ := initGitRepoWithStager(t)
	if err := RunStructuralChecks(root); err != nil {
		t.Fatalf("expected nil on a tree with nothing to flag, got: %v", err)
	}
}

// TestStructuralChecks_PinsTheSet asserts the exact contents of the list.
//
// Every other test in this section *iterates* structuralChecks, which means a
// deleted entry makes them all assert less while still passing — precisely the
// shape of the defect this list exists to prevent, where a check's caller went
// away and nothing said so (T2160). Removing `host-tool-lookups` and running the
// suite is green without this test.
//
// The duplicated list below is the assertion, not redundant state: it is what
// "the set changed" is measured against, so a future reader must NOT "simplify"
// it by ranging over structuralChecks — that makes the test vacuous. Changing
// the set is allowed; doing it without noticing is not.
func TestStructuralChecks_PinsTheSet(t *testing.T) {
	want := []struct {
		name  string
		check func(string) error
	}{
		{"docs", CheckDocs},
		{"test-sleeps", CheckTestSleeps},
		{"test-temp-paths", CheckTestTempPaths},
		{"host-tool-lookups", CheckHostToolLookups},
	}

	var gotNames []string
	for _, c := range structuralChecks {
		gotNames = append(gotNames, c.name)
	}
	if len(structuralChecks) != len(want) {
		t.Fatalf("structuralChecks has %d entries %v, want %d — a sweep was added or "+
			"removed; update this test deliberately, and the table in docs/build-tools.md "+
			"§\"The structural sweeps\" with it", len(structuralChecks), gotNames, len(want))
	}
	for i, w := range want {
		got := structuralChecks[i]
		if got.name != w.name {
			t.Errorf("structuralChecks[%d] is named %q, want %q", i, got.name, w.name)
			continue
		}
		// Function values are not comparable, so compare code pointers: this
		// catches an entry renamed onto the wrong check, which a name-only
		// assertion would pass.
		if reflect.ValueOf(got.check).Pointer() != reflect.ValueOf(w.check).Pointer() {
			t.Errorf("structuralChecks[%d] %q does not call the function it names", i, got.name)
		}
	}
}

// The table in docs/build-tools.md §"The structural sweeps" names every sweep a
// verify run enforces, so a sweep added to the list without a row there leaves
// the document describing a gate that is stricter than it says. Reconciled here
// rather than by prose, for the same reason the annotation set and the doc index
// are reconciled mechanically: a table nobody checks is a table that drifts.
func TestStructuralChecks_DocumentedInBuildTools(t *testing.T) {
	const doc = "docs/build-tools.md"
	root := filepath.Join("..", "..", "..")
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(doc)))
	if err != nil {
		t.Fatalf("read %s: %v", doc, err)
	}

	// The rows between the table header and the blank line that ends it.
	body := string(data)
	at := strings.Index(body, "### The structural sweeps")
	if at < 0 {
		t.Fatalf("%s has no \"### The structural sweeps\" section", doc)
	}
	documented := map[string]bool{}
	inTable := false
	for _, line := range strings.Split(body[at:], "\n") {
		switch {
		case strings.HasPrefix(line, "| Name |"):
			inTable = true
		case inTable && !strings.HasPrefix(line, "|"):
			inTable = false
		case inTable && strings.HasPrefix(line, "| `"):
			if end := strings.Index(line[3:], "`"); end >= 0 {
				documented[line[3:3+end]] = true
			}
		}
	}

	for _, c := range structuralChecks {
		if !documented[c.name] {
			t.Errorf("sweep %q has no row in %s §\"The structural sweeps\"", c.name, doc)
		}
		delete(documented, c.name)
	}
	for name := range documented {
		t.Errorf("%s documents a sweep %q that structuralChecks does not run", doc, name)
	}
}

// One failing sweep reports as one line of context, with no blank line from
// errors.Join's separator and the sweep's own name in front of it.
func TestRunStructuralChecks_SingleFailureIsNotPadded(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	stage("tests/only_test.pr", []byte("test_x() `test { sleep(1ms); }\n"))

	err := RunStructuralChecks(root)
	if err == nil {
		t.Fatal("expected an error for an unannotated sleep(), got nil")
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "test-sleeps: ") {
		t.Errorf("message should lead with the failing sweep's name, got:\n%v", msg)
	}
	if strings.HasSuffix(msg, "\n") || strings.Contains(msg, "\n\n") {
		t.Errorf("a single failure must not be padded by the join separator, got:\n%q", msg)
	}
}
