package common

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
)

// windowsCommandLineLimit is the hard CreateProcessW limit that T1582 tripped.
const windowsCommandLineLimit = 32767

// renderedLen recomputes the command-line cost of `exe fixed... batch...` using
// the same upper-bound model as the chunker, so tests assert against the budget
// independently of how commandLineBatches accumulates internally.
func renderedLen(exe string, fixed, batch []string) int {
	n := argCost(exe)
	for _, a := range fixed {
		n += argCost(a)
	}
	for _, a := range batch {
		n += argCost(a)
	}
	return n
}

func flatten(batches [][]string) []string {
	var all []string
	for _, b := range batches {
		all = append(all, b...)
	}
	return all
}

func TestCommandLineBatchesRespectsBudget(t *testing.T) {
	exe := `C:\Users\someone\prog\win_promise_1\bin\promise.exe`
	fixed := []string{"format"}

	files := make([]string, 2000)
	for i := range files {
		files[i] = fmt.Sprintf("modules/std/very_long_directory_name_%04d/source_file_%04d.pr", i, i)
	}
	joined := len(strings.Join(files, " "))
	if joined <= windowsCommandLineLimit {
		t.Fatalf("test input too small to exercise the bug: %d chars", joined)
	}

	batches := commandLineBatches(exe, fixed, files, maxCommandLine)
	if len(batches) < 2 {
		t.Fatalf("expected the oversized list to split, got %d batch(es)", len(batches))
	}
	for i, b := range batches {
		if len(b) == 0 {
			t.Errorf("batch %d is empty", i)
		}
		if got := renderedLen(exe, fixed, b); got > maxCommandLine {
			t.Errorf("batch %d rendered %d chars, over budget %d", i, got, maxCommandLine)
		}
	}

	// Union equals the input exactly: order preserved, nothing dropped, nothing
	// duplicated (slice equality catches all three at once).
	if got := flatten(batches); !reflect.DeepEqual(got, files) {
		t.Errorf("batches do not reconstruct the input: got %d paths, want %d", len(got), len(files))
	}
}

func TestCommandLineBatchesOversizePathGetsOwnBatch(t *testing.T) {
	exe := "promise"
	fixed := []string{"format"}
	huge := strings.Repeat("x", maxCommandLine+500) + ".pr"
	files := []string{"a.pr", huge, "b.pr"}

	batches := commandLineBatches(exe, fixed, files, maxCommandLine)
	if got := flatten(batches); !reflect.DeepEqual(got, files) {
		t.Fatalf("oversize path dropped or reordered: got %d paths, want %d", len(got), len(files))
	}
	// The huge path must be alone in its batch — it cannot share with anything.
	var found bool
	for _, b := range batches {
		for _, f := range b {
			if f == huge {
				found = true
				if len(b) != 1 {
					t.Errorf("oversize path shares a batch with %d other file(s)", len(b)-1)
				}
			}
		}
	}
	if !found {
		t.Fatal("oversize path missing from batches")
	}
}

// TestCommandLineBatchesChargesExeAndFixedArgs guards the exact bug class T1582
// was: a list that fits on its own, but not once the absolute exe path and the
// `format -check` flags are prepended. The budget must cover the whole rendered
// command line, not just the file arguments.
func TestCommandLineBatchesChargesExeAndFixedArgs(t *testing.T) {
	exe := `C:\` + strings.Repeat("d", 200) + `\bin\promise.exe`
	fixed := []string{"format", "-check"}
	files := []string{"a.pr", "b.pr", "c.pr"}

	// A budget with room for the fixed part plus exactly one file.
	budget := renderedLen(exe, fixed, files[:1])

	// Sanity: the files alone fit easily — only the fixed cost forces a split,
	// so this test fails if the chunker ever stops charging it.
	if filesOnly := renderedLen("", nil, files); filesOnly > budget {
		t.Fatalf("test setup wrong: files alone (%d) already exceed budget %d", filesOnly, budget)
	}

	batches := commandLineBatches(exe, fixed, files, budget)
	if len(batches) != len(files) {
		t.Fatalf("expected one file per batch, got %d batch(es) for %d files", len(batches), len(files))
	}
	if got := flatten(batches); !reflect.DeepEqual(got, files) {
		t.Errorf("batches do not reconstruct the input: %v", got)
	}
}

func TestCommandLineBatchesEmptyAndSingle(t *testing.T) {
	if got := commandLineBatches("promise", []string{"format"}, nil, maxCommandLine); got != nil {
		t.Errorf("empty input should produce no batches, got %v", got)
	}
	got := commandLineBatches("promise", []string{"format"}, []string{"a.pr"}, maxCommandLine)
	if len(got) != 1 || !reflect.DeepEqual(got[0], []string{"a.pr"}) {
		t.Errorf("single small file should produce one batch, got %v", got)
	}
}

// TestCommandLineBatchesRepoScaleUnderWindowsLimit is the regression guard at the
// real shape that broke: a repo-scale .pr list plus a long absolute exe path must
// split into batches that each stay strictly under CreateProcessW's hard limit.
func TestCommandLineBatchesRepoScaleUnderWindowsLimit(t *testing.T) {
	exe := `C:\Users\a_developer_with_a_long_name\prog\win_promise_1\bin\promise.exe`
	fixed := []string{"format", "-check"}

	files := make([]string, 1000)
	for i := range files {
		files[i] = fmt.Sprintf(`tests\concurrency\stress_channel_case_%03d.pr`, i)
	}

	batches := commandLineBatches(exe, fixed, files, maxCommandLine)
	if len(batches) < 2 {
		t.Fatalf("expected >1 batch at repo scale, got %d", len(batches))
	}
	for i, b := range batches {
		if got := renderedLen(exe, fixed, b); got >= windowsCommandLineLimit {
			t.Errorf("batch %d rendered %d chars, at/over the CreateProcessW limit %d", i, got, windowsCommandLineLimit)
		}
	}
	if got := flatten(batches); !reflect.DeepEqual(got, files) {
		t.Errorf("batches do not reconstruct the input")
	}
}

// TestCommandLineBatchesExactFitDoesNotSplit pins the boundary: a batch whose
// rendered length lands exactly on the budget still fits. An off-by-one here
// would silently double the number of subprocesses on every run.
func TestCommandLineBatchesExactFitDoesNotSplit(t *testing.T) {
	exe := "promise"
	fixed := []string{"format"}
	files := []string{"aaa.pr", "bbb.pr", "ccc.pr"}

	exact := renderedLen(exe, fixed, files)
	batches := commandLineBatches(exe, fixed, files, exact)
	if len(batches) != 1 {
		t.Errorf("budget exactly equal to the rendered length must not split, got %d batches", len(batches))
	}

	// One character less must split — confirms `exact` really was the boundary
	// and the single-batch result above wasn't slack in the estimate.
	if got := commandLineBatches(exe, fixed, files, exact-1); len(got) != 2 {
		t.Errorf("budget one below the rendered length should split into 2, got %d", len(got))
	}
}

// manyFiles builds a .pr list guaranteed to need more than one batch.
func manyFiles(n int) []string {
	files := make([]string, n)
	for i := range files {
		files[i] = fmt.Sprintf("modules/std/a_reasonably_long_module_path_%04d/file_%04d.pr", i, i)
	}
	return files
}

func TestUnformattedPromiseFilesAggregatesBatches(t *testing.T) {
	files := manyFiles(1500)

	var seen []string
	batchIndex := 0
	run := func(args []string) (string, error) {
		if len(args) < 2 || args[0] != "format" || args[1] != "-check" {
			t.Errorf("batch %d: unexpected fixed args %v", batchIndex, args[:min(2, len(args))])
		}
		seen = append(seen, args[2:]...)
		batchIndex++
		// Every batch reports its own first file as unformatted, exiting non-zero
		// — the normal "unformatted" signal.
		return args[2] + "\n", errors.New("exit status 1")
	}

	got, err := unformattedPromiseFilesWith("promise", files, run)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if batchIndex < 2 {
		t.Fatalf("expected >1 batch, got %d", batchIndex)
	}
	if len(got) != batchIndex {
		t.Errorf("expected one reported file per batch (%d), got %d: %v", batchIndex, len(got), got)
	}
	if !reflect.DeepEqual(seen, files) {
		t.Errorf("runner did not see each input file exactly once, in order (saw %d, want %d)", len(seen), len(files))
	}
}

func TestUnformattedPromiseFilesBatchFailure(t *testing.T) {
	files := manyFiles(1500)

	// Empty stdout + non-zero exit on the second batch is a genuine failure.
	calls := 0
	_, err := unformattedPromiseFilesWith("promise", files, func(args []string) (string, error) {
		calls++
		if calls == 2 {
			return "", errors.New("boom")
		}
		return "", nil
	})
	if err == nil {
		t.Fatal("expected a genuine batch failure to surface")
	}
	if !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "batch 2/") {
		t.Errorf("error should name the failing batch and cause, got: %v", err)
	}

	// Output + non-zero exit is the unformatted signal, not an error.
	got, err := unformattedPromiseFilesWith("promise", files, func(args []string) (string, error) {
		return "  " + args[2] + "  \n\n", errors.New("exit status 1")
	})
	if err != nil {
		t.Fatalf("non-zero exit with output must not be an error, got: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("expected unformatted paths to be reported")
	}
	for _, f := range got {
		if f != strings.TrimSpace(f) || f == "" {
			t.Errorf("reported path not trimmed: %q", f)
		}
	}
}

func TestFormatPromiseFilesBatchesAllFiles(t *testing.T) {
	files := manyFiles(1500)

	var seen []string
	batches := 0
	err := formatPromiseFilesWith("promise", files, func(args []string) (string, error) {
		if len(args) < 2 || args[0] != "format" {
			t.Errorf("unexpected fixed args %v", args[:min(1, len(args))])
		}
		seen = append(seen, args[1:]...)
		batches++
		return "", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if batches < 2 {
		t.Fatalf("expected >1 batch, got %d", batches)
	}
	if !reflect.DeepEqual(seen, files) {
		t.Errorf("not every file was formatted exactly once (saw %d, want %d)", len(seen), len(files))
	}

	// A failing batch aborts the remaining batches.
	calls := 0
	err = formatPromiseFilesWith("promise", files, func(args []string) (string, error) {
		calls++
		return "", errors.New("boom")
	})
	if err == nil {
		t.Fatal("expected the batch failure to surface")
	}
	if calls != 1 {
		t.Errorf("expected to abort after the first failing batch, ran %d", calls)
	}
	if !strings.Contains(err.Error(), "batch 1/") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should name the failing batch and cause, got: %v", err)
	}
}

// --- End-to-end: real subprocess, real .pr discovery -------------------------
//
// The tests above exercise the chunker with an injected runner. These drive the
// exported entry points (FormatPromiseFiles for bin/format + bin/verify,
// UnformattedPromiseFiles for the pre-commit gate) all the way through
// findPromiseFiles and a real os/exec spawn, with a repo large enough that an
// unbatched command line would exceed CreateProcessW's limit. On Windows a
// regression here fails with the original ERROR_FILENAME_EXCED_RANGE (T1582);
// on POSIX it still catches dropped, duplicated, or misrouted paths.

// promiseStubSource stands in for bin/promise. It appends one tab-separated
// line per invocation to $PROMISE_STUB_LOG recording that batch's file
// arguments, so a test can reconstruct exactly how the list was split. Outcome
// is selected by $PROMISE_STUB_MODE: unset formats successfully; "check"
// reports the batch's first file as unformatted and exits non-zero (what
// `format -check` does when files need formatting); "fail" exits non-zero with
// no output (a genuine tool failure).
const promiseStubSource = `package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	var files []string
	for _, a := range os.Args[1:] {
		if a == "format" || strings.HasPrefix(a, "-") {
			continue
		}
		files = append(files, a)
	}
	if logPath := os.Getenv("PROMISE_STUB_LOG"); logPath != "" {
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, "stub: "+err.Error())
			os.Exit(3)
		}
		fmt.Fprintln(f, strings.Join(files, "\t"))
		f.Close()
	}
	switch os.Getenv("PROMISE_STUB_MODE") {
	case "check":
		if len(files) > 0 {
			fmt.Println(files[0])
		}
		os.Exit(1)
	case "fail":
		os.Exit(1)
	}
}
`

var (
	promiseStubOnce sync.Once
	promiseStubPath string
	promiseStubErr  error
)

// promiseStub compiles promiseStubSource once per test binary and returns the
// executable's path. The temp dir is deliberately not removed — the executable
// must outlive this call so tests can copy and exec it (same approach as
// teeStub in exec_test.go).
func promiseStub(t *testing.T) string {
	t.Helper()
	promiseStubOnce.Do(func() {
		dir, err := os.MkdirTemp("", "promisestub-")
		if err != nil {
			promiseStubErr = err
			return
		}
		src := filepath.Join(dir, "main.go")
		if err := os.WriteFile(src, []byte(promiseStubSource), 0o644); err != nil {
			promiseStubErr = err
			return
		}
		exe := filepath.Join(dir, "promisestub"+ExeSuffix())
		cmd := exec.Command("go", "build", "-o", exe, src)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			promiseStubErr = fmt.Errorf("go build promise stub: %w", err)
			return
		}
		promiseStubPath = exe
	})
	if promiseStubErr != nil {
		t.Fatalf("build promise stub: %v", promiseStubErr)
	}
	return promiseStubPath
}

// installPromiseStub copies the stub to <root>/bin/promise[.exe], where
// FormatPromiseFiles and UnformattedPromiseFiles look for the compiler, and
// points the stub's log at a fresh file. Returns the binary and log paths.
func installPromiseStub(t *testing.T, root, mode string) (bin, logPath string) {
	t.Helper()
	src, err := os.ReadFile(promiseStub(t))
	if err != nil {
		t.Fatalf("read stub: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	bin = filepath.Join(root, "bin", BinaryName())
	if err := os.WriteFile(bin, src, 0o755); err != nil {
		t.Fatalf("install stub: %v", err)
	}
	logPath = filepath.Join(t.TempDir(), "batches.log")
	t.Setenv("PROMISE_STUB_LOG", logPath)
	t.Setenv("PROMISE_STUB_MODE", mode)
	return bin, logPath
}

// writePromiseRepo creates n .pr files under root, plus one .pr file inside each
// directory findPromiseFiles must skip. Returns the sorted repo-relative paths
// that should be handed to the formatter.
func writePromiseRepo(t *testing.T, root string, n int) []string {
	t.Helper()
	const perDir = 40
	var want []string
	for i := range n {
		dir := fmt.Sprintf("modules/generated_module_group_%04d", i/perDir)
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		rel := filepath.Join(dir, fmt.Sprintf("a_long_generated_source_file_%04d.pr", i))
		if err := os.WriteFile(filepath.Join(root, rel), []byte("main() {}\n"), 0o644); err != nil {
			t.Fatalf("write .pr: %v", err)
		}
		want = append(want, rel)
	}
	// findPromiseFiles excludes .git/, compiler/, .promise-home/ and any hidden
	// directory; a .pr in each must never reach the formatter.
	for _, skipped := range []string{".git", "compiler", ".promise-home", ".hidden"} {
		if err := os.MkdirAll(filepath.Join(root, skipped), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", skipped, err)
		}
		if err := os.WriteFile(filepath.Join(root, skipped, "excluded.pr"), []byte("main() {}\n"), 0o644); err != nil {
			t.Fatalf("write excluded .pr: %v", err)
		}
	}
	sort.Strings(want)
	return want
}

// readStubBatches parses the stub's log into the per-invocation file lists.
func readStubBatches(t *testing.T, logPath string) [][]string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read stub log (was the stub ever invoked?): %v", err)
	}
	var batches [][]string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			batches = append(batches, strings.Split(line, "\t"))
		}
	}
	return batches
}

// oversizedRepoFileCount yields a .pr list whose single-command-line rendering
// comfortably exceeds the 32,767-character CreateProcessW limit (~72 chars per
// path × 600 ≈ 45,000), so batching is genuinely required.
const oversizedRepoFileCount = 600

func TestFormatPromiseFiles_RealSubprocessBatchesEveryFile(t *testing.T) {
	root := t.TempDir()
	bin, logPath := installPromiseStub(t, root, "")
	want := writePromiseRepo(t, root, oversizedRepoFileCount)

	if joined := len(strings.Join(want, " ")); joined <= windowsCommandLineLimit {
		t.Fatalf("repo too small to require batching: %d chars", joined)
	}

	if err := FormatPromiseFiles(root, bin); err != nil {
		t.Fatalf("FormatPromiseFiles: %v", err)
	}

	batches := readStubBatches(t, logPath)
	if len(batches) < 2 {
		t.Fatalf("expected the oversized list to be split, got %d invocation(s)", len(batches))
	}
	got := flatten(batches)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("formatter did not see every .pr file exactly once: got %d paths, want %d", len(got), len(want))
	}
	for _, f := range got {
		if strings.Contains(f, "excluded.pr") {
			t.Errorf("excluded path reached the formatter: %s", f)
		}
	}
}

func TestUnformattedPromiseFiles_RealSubprocessAggregatesBatches(t *testing.T) {
	root := t.TempDir()
	_, logPath := installPromiseStub(t, root, "check")
	writePromiseRepo(t, root, oversizedRepoFileCount)

	// Every batch reports one unformatted file and exits non-zero. The gate must
	// treat that as the "unformatted" signal and collect a result per batch —
	// before batching, only one invocation happened and only its files were seen.
	got, err := UnformattedPromiseFiles(root)
	if err != nil {
		t.Fatalf("non-zero exit with output must not be an error: %v", err)
	}
	batches := readStubBatches(t, logPath)
	if len(batches) < 2 {
		t.Fatalf("expected >1 invocation, got %d", len(batches))
	}
	if len(got) != len(batches) {
		t.Errorf("expected one reported file per batch (%d), got %d: %v", len(batches), len(got), got)
	}
	for i, b := range batches {
		if i < len(got) && got[i] != b[0] {
			t.Errorf("batch %d reported %q, gate returned %q", i, b[0], got[i])
		}
	}
}

// TestUnformattedPromiseFiles_RealSubprocessGenuineFailure covers the other half
// of the exit-code split: a non-zero exit with NO output is a real failure and
// must surface, naming the batch, rather than being read as "nothing unformatted".
func TestUnformattedPromiseFiles_RealSubprocessGenuineFailure(t *testing.T) {
	root := t.TempDir()
	installPromiseStub(t, root, "fail")
	writePromiseRepo(t, root, 3)

	got, err := UnformattedPromiseFiles(root)
	if err == nil {
		t.Fatalf("expected a genuine failure to surface, got %v", got)
	}
	if !strings.Contains(err.Error(), "promise format -check") || !strings.Contains(err.Error(), "batch 1/") {
		t.Errorf("error should name the operation and failing batch, got: %v", err)
	}
}

// TestFormatPromiseFiles_BatchFailureIsBrief checks the RunInBrief wiring end to
// end: when a batch of hundreds of files fails, the reported error stays short
// instead of echoing the whole argument list back (the noise that obscured the
// original T1582 failure).
func TestFormatPromiseFiles_BatchFailureIsBrief(t *testing.T) {
	root := t.TempDir()
	bin, _ := installPromiseStub(t, root, "fail")
	want := writePromiseRepo(t, root, oversizedRepoFileCount)

	err := FormatPromiseFiles(root, bin)
	if err == nil {
		t.Fatal("expected the failing batch to surface")
	}
	msg := err.Error()
	if !strings.Contains(msg, "batch 1/") || !strings.Contains(msg, "args)") {
		t.Errorf("error should name the batch and its argument count, got: %v", msg)
	}
	if strings.Contains(msg, want[len(want)/2]) {
		t.Errorf("error splices the file list back into the message: %q", msg)
	}
	if len(msg) > 500 {
		t.Errorf("error message is %d chars, want a brief summary", len(msg))
	}
}

// TestUnformattedPromiseFiles_NoPromiseFilesSkipsSubprocess mirrors the format
// side: with the compiler present but nothing to check, the gate reports nothing
// unformatted without spawning a batch (which, with an empty list, would have
// been `promise format -check` over the whole repo).
func TestUnformattedPromiseFiles_NoPromiseFilesSkipsSubprocess(t *testing.T) {
	root := t.TempDir()
	_, logPath := installPromiseStub(t, root, "fail") // would fail if ever run

	got, err := UnformattedPromiseFiles(root)
	if err != nil {
		t.Fatalf("empty repo should be a no-op, got: %v", err)
	}
	if got != nil {
		t.Errorf("expected no unformatted files, got %v", got)
	}
	if _, err := os.Stat(logPath); err == nil {
		t.Error("format -check was invoked despite there being no .pr files")
	}
}

func TestFormatPromiseFiles_NoPromiseFilesSkipsSubprocess(t *testing.T) {
	root := t.TempDir()
	bin, logPath := installPromiseStub(t, root, "fail") // would fail if ever run
	if err := FormatPromiseFiles(root, bin); err != nil {
		t.Fatalf("empty repo should be a no-op, got: %v", err)
	}
	if _, err := os.Stat(logPath); err == nil {
		t.Error("formatter was invoked despite there being no .pr files")
	}
}

// TestFormatPromiseFiles_WalkErrorIsWrapped covers the discovery error path —
// a root that does not exist must be reported as a find failure, not silently
// treated as "no files to format".
func TestFormatPromiseFiles_WalkErrorIsWrapped(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	err := FormatPromiseFiles(missing, filepath.Join(missing, "bin", BinaryName()))
	if err == nil {
		t.Fatal("expected an error for a missing root")
	}
	if !strings.Contains(err.Error(), "find .pr files") {
		t.Errorf("error should identify the discovery step, got: %v", err)
	}
}

// TestFindPromiseFilesExcludesGeneratedAndHiddenDirs pins the exclusion policy
// the chunker inherits: whatever is skipped here never reaches a batch. The
// compiler/ tree in particular holds thousands of .pr test fixtures that must
// not be formatted.
func TestFindPromiseFilesExcludesGeneratedAndHiddenDirs(t *testing.T) {
	root := t.TempDir()
	want := writePromiseRepo(t, root, 3)

	// Nested exclusions too — the walk must SkipDir the whole subtree.
	if err := os.MkdirAll(filepath.Join(root, "compiler", "internal", "codegen"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "compiler", "internal", "codegen", "deep.pr"), []byte("main() {}\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Non-.pr files are ignored regardless of location.
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := findPromiseFiles(root)
	if err != nil {
		t.Fatalf("findPromiseFiles: %v", err)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("findPromiseFiles = %v, want %v", got, want)
	}
}

// --- FormatGo: the repair mode of the formatted:go pair ---
//
// UnformattedGoFiles, its measure mode, is covered in precommit_test.go. These
// cover the half that WRITES: what it produces, what it leaves alone, and that
// the two modes agree about which files are subject to formatting at all.

// goSourceRoot writes files under compiler/, which is where goFileDirs looks.
func goSourceRoot(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		full := filepath.Join(root, "compiler", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// What the repair mode does: rewrite exactly the files gofmt would change, and
// touch nothing else. A formatter that rewrote an already-formatted file would
// show up as a spurious diff on every run.
func TestFormatGo_RewritesOnlyWhatNeedsIt(t *testing.T) {
	const formatted = "package a\n\nfunc F() {}\n"
	root := goSourceRoot(t, map[string]string{
		"a.go":         formatted,
		"b.go":         "package b\nfunc  G(){}\n",
		"vendor/c.go":  "package c\nfunc  H(){}\n",
		"notes.txt":    "func  not_go(){}\n",
		"unparsed.go":  "package d\nfunc  {{{\n",
		".hidden/e.go": "package e\nfunc  I(){}\n",
	})

	if err := FormatGo(root); err != nil {
		t.Fatalf("FormatGo: %v", err)
	}

	at := func(rel string) string { return filepath.Join(root, "compiler", filepath.FromSlash(rel)) }
	if got := readFile(t, at("b.go")); got != "package b\n\nfunc G() {}\n" {
		t.Errorf("b.go = %q, want it reformatted", got)
	}
	// Everything else is byte-for-byte what it was.
	for rel, want := range map[string]string{
		"a.go":         formatted,
		"vendor/c.go":  "package c\nfunc  H(){}\n",
		"notes.txt":    "func  not_go(){}\n",
		"unparsed.go":  "package d\nfunc  {{{\n",
		".hidden/e.go": "package e\nfunc  I(){}\n",
	} {
		if got := readFile(t, at(rel)); got != want {
			t.Errorf("%s = %q, want it untouched (%q)", rel, got, want)
		}
	}
}

// On Windows git is often configured with core.autocrlf=true, so a CRLF file
// rewritten with LF endings is a whole-file diff that nobody asked for. The
// formatter compares line-ending-agnostically and writes back the endings it
// found.
func TestFormatGo_PreservesCRLF(t *testing.T) {
	root := goSourceRoot(t, map[string]string{"b.go": "package b\r\nfunc  G(){}\r\n"})
	if err := FormatGo(root); err != nil {
		t.Fatalf("FormatGo: %v", err)
	}
	got := readFile(t, filepath.Join(root, "compiler", "b.go"))
	if want := "package b\r\n\r\nfunc G() {}\r\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// The file's mode is the author's, not the formatter's: rewriting must not
// quietly widen or narrow it.
func TestFormatGo_PreservesFileMode(t *testing.T) {
	root := goSourceRoot(t, map[string]string{"b.go": "package b\nfunc  G(){}\n"})
	path := filepath.Join(root, "compiler", "b.go")
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	// The mode to preserve is the one the filesystem actually recorded, not the
	// one Chmod was handed: Windows keeps no POSIX permission bits and reports
	// 0666 for any writable file, so asserting a literal 0600 there asserts a bit
	// pattern the platform can never produce (T2094). Reading it back makes this
	// test preservation - the property it is named for - on every platform, and
	// on POSIX it is the same assertion as before, since 0600 is what comes back.
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := FormatGo(root); err != nil {
		t.Fatalf("FormatGo: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := after.Mode().Perm(), before.Mode().Perm(); got != want {
		t.Errorf("mode = %v, want %v", got, want)
	}
}

// A directory the tree does not have is not an error. bin/verify runs this over
// real repositories and tests run it over temp roots that have only some of the
// directories; one mode tolerating that and the other not is how the pair came
// to disagree once already.
func TestFormatGo_MissingDirectoryIsNotAnError(t *testing.T) {
	root := t.TempDir() // no compiler/, no tools/build/
	if err := FormatGo(root); err != nil {
		t.Errorf("FormatGo: %v", err)
	}
	files, err := UnformattedGoFiles(root)
	if err != nil {
		t.Errorf("UnformattedGoFiles: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("got %v, want none", files)
	}
}

// The pair property, over one tree: the files the gate NAMES are exactly the
// files the tool REWRITES, and once it has, the gate names none. Compared by
// driving both rather than by spelling a list here — a hand-written expectation
// is a third answer, and would go on passing while the two it describes drifted
// (T2104).
func TestFormatGo_RewritesExactlyWhatUnformattedGoFilesNames(t *testing.T) {
	root := goSourceRoot(t, map[string]string{
		"a.go":           "package a\n\nfunc F() {}\n",
		"b.go":           "package b\nfunc  G(){}\n",
		"deep/c.go":      "package c\nfunc   H()  {}\n",
		"deep/more/d.go": "package d\n\nfunc I() {}\n",
		"unparseable.go": "package e\nfunc  {{{\n",
		"vendor/skip.go": "package f\nfunc  J(){}\n",
	})

	named, err := UnformattedGoFiles(root)
	if err != nil {
		t.Fatalf("UnformattedGoFiles: %v", err)
	}
	if len(named) == 0 {
		t.Fatal("the fixture produced no unformatted files, so every pin here would be vacuous")
	}

	before := map[string]string{}
	for _, rel := range named {
		before[rel] = readFile(t, filepath.Join(root, rel))
	}

	if err := FormatGo(root); err != nil {
		t.Fatalf("FormatGo: %v", err)
	}

	// Every named file changed...
	for rel, was := range before {
		if readFile(t, filepath.Join(root, rel)) == was {
			t.Errorf("%s was named unformatted but FormatGo left it alone", rel)
		}
	}
	// ...and nothing is named any more.
	after, err := UnformattedGoFiles(root)
	if err != nil {
		t.Fatalf("UnformattedGoFiles: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("after FormatGo the gate still names %v", after)
	}
	// Running the repair mode again is a no-op, which is what makes it safe for
	// bin/verify to run it before every measurement.
	if err := FormatGo(root); err != nil {
		t.Fatalf("second FormatGo: %v", err)
	}
	if again, _ := UnformattedGoFiles(root); len(again) != 0 {
		t.Errorf("a second FormatGo made work for the gate: %v", again)
	}
}

// A walk that could not read part of the tree has not found zero unformatted
// files — it has not looked. Both modes must fail rather than report a clean
// tree, which is the same false-clean the checked:go pair was fixed for (T2104).
func TestFormatGo_AnUnreadableDirectoryIsAnErrorNotACleanTree(t *testing.T) {
	// The condition has to be induced with POSIX permission bits, and only a
	// platform that has them can hold it. Windows keeps none - os.Chmod there
	// toggles the read-only attribute, which does not stop a directory being
	// listed - so the walk would succeed and this test would assert the opposite
	// of what it is named for. The platform check has to come first, because
	// os.Geteuid reports -1 on Windows and so never trips the root skip (T2094).
	//
	// A Windows-side induction does exist (open the file with no sharing, so the
	// walk's ReadFile fails), but it is platform-specific test machinery for a
	// property this file tests adequately elsewhere; skipping is the honest
	// outcome rather than asserting something weaker and calling it covered.
	if runtime.GOOS == "windows" {
		t.Skip("windows has no POSIX directory permission bits: an unreadable directory cannot be induced")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: permissions do not restrict the walk")
	}
	root := goSourceRoot(t, map[string]string{"deep/b.go": "package b\nfunc  G(){}\n"})
	locked := filepath.Join(root, "compiler", "deep")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) }) // so t.TempDir can clean up

	if _, err := UnformattedGoFiles(root); err == nil {
		t.Error("the gate reported a tree it could not read as having nothing unformatted")
	}
	if err := FormatGo(root); err == nil {
		t.Error("the tool reported a tree it could not read as formatted")
	}
}
