package common

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// setupGitRepo creates a temp git repo with an initial baselines.json commit,
// then stages a modified baselines.json. Returns the repo root.
func setupGitRepo(t *testing.T, headBaselines, stagedBaselines Baselines) string {
	t.Helper()
	root := t.TempDir()

	// Init git repo.
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

	// Create baselines.json under the expected path and commit.
	gatesDir := filepath.Join(root, "tools", "gates")
	os.MkdirAll(gatesDir, 0o755)
	blPath := filepath.Join(gatesDir, "baselines.json")

	headData, _ := json.MarshalIndent(headBaselines, "", "  ")
	headData = append(headData, '\n')
	os.WriteFile(blPath, headData, 0o644)

	git("add", "tools/gates/baselines.json")
	git("commit", "-m", "initial baselines")

	// Write staged version.
	stagedData, _ := json.MarshalIndent(stagedBaselines, "", "  ")
	stagedData = append(stagedData, '\n')
	os.WriteFile(blPath, stagedData, 0o644)

	git("add", "tools/gates/baselines.json")

	return root
}

// initGitRepoWithStagedFile creates a temp git repo with one initial commit,
// then writes and stages a file at relPath. Returns the repo root.
func initGitRepoWithStagedFile(t *testing.T, relPath string) string {
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

	// Seed an initial commit so HEAD exists.
	os.WriteFile(filepath.Join(root, "seed.txt"), []byte("seed\n"), 0o644)
	git("add", "seed.txt")
	git("commit", "-m", "seed")

	full := filepath.Join(root, relPath)
	os.MkdirAll(filepath.Dir(full), 0o755)
	os.WriteFile(full, []byte("data\n"), 0o644)
	git("add", relPath)

	return root
}

// initGitRepoWithStager creates a temp git repo with one seed commit and a
// noreply identity, and returns the repo root plus a stage function that writes
// arbitrary bytes at a repo-relative path and `git add`s it. Used by the T1620
// binary/size gate tests, which need to stage exact byte content (including
// NUL) at arbitrary paths.
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

// TestRunPreCommit_RejectsBinaryAtUndeclaredPath: a blob with a NUL byte in its
// first 8 KB, at a path not declared binary in .gitattributes, is rejected.
func TestRunPreCommit_RejectsBinaryAtUndeclaredPath(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	stage("assets/blob.dat", []byte{'a', 0x00, 'b'})
	if err := RunPreCommit(root); err == nil {
		t.Fatal("expected error for binary content at undeclared path, got nil")
	}
}

// TestRunPreCommit_AllowsBinaryAtDeclaredPath: a binary blob is admitted when
// .gitattributes declares its path binary — via either `binary` (attribute set)
// or `-text` (text unset). Both are the repo's real escape hatches.
func TestRunPreCommit_AllowsBinaryAtDeclaredPath(t *testing.T) {
	for _, decl := range []string{"assets/*.dat binary", "assets/*.dat -text"} {
		t.Run(decl, func(t *testing.T) {
			root, stage := initGitRepoWithStager(t)
			stage(".gitattributes", []byte(decl+"\n"))
			stage("assets/blob.dat", []byte{'a', 0x00, 'b'})
			if err := RunPreCommit(root); err != nil {
				t.Fatalf("expected no error for binary at declared path (%q), got: %v", decl, err)
			}
		})
	}
}

// TestRunPreCommit_RejectsOversizedTextFile: the size gate is independent of the
// binary gate — a large all-text (no NUL) blob is still rejected.
func TestRunPreCommit_RejectsOversizedTextFile(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	stage("data/big.json", bytes.Repeat([]byte{'a'}, maxCommittedFileSize+1))
	if err := RunPreCommit(root); err == nil {
		t.Fatal("expected error for oversized text file, got nil")
	}
}

// TestRunPreCommit_AllowsOneMegabyteTextFile: a blob of exactly the limit
// passes — the boundary is inclusive.
func TestRunPreCommit_AllowsOneMegabyteTextFile(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	stage("data/exact.json", bytes.Repeat([]byte{'a'}, maxCommittedFileSize))
	if err := RunPreCommit(root); err != nil {
		t.Fatalf("expected no error at exactly the size limit, got: %v", err)
	}
}

// TestRunPreCommit_RejectsOversizedDeclaredBinary: the size gate is independent
// of .gitattributes. A blob declared binary (so the content gate would admit it)
// that is over the limit is still rejected — the acceptance criterion "a >1 MB
// file fails ... independently of .gitattributes". Guards against the size check
// being folded into the same escape hatch as the binary-content check.
func TestRunPreCommit_RejectsOversizedDeclaredBinary(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	stage(".gitattributes", []byte("assets/*.dat binary\n"))
	// Over the limit AND full of NUL — the content gate would admit it via the
	// declaration, so the only thing that can reject it is the size gate.
	stage("assets/blob.dat", bytes.Repeat([]byte{0x00}, maxCommittedFileSize+1))
	if err := RunPreCommit(root); err == nil {
		t.Fatal("expected error — an oversized blob must fail the size gate even when declared binary")
	}
}

// TestRunPreCommit_AllowsKnownBinariesViaRealGitattributes stages the repo's
// actual .gitattributes escape-hatch lines and a binary blob at each of the
// three real committed-binary paths, asserting all pass. Unlike
// TestRunPreCommit_AllowsBinaryAtDeclaredPath (which uses a synthetic
// assets/*.dat pattern to test the mechanism), this pins the real glob patterns
// to the real filenames — a typo like '*.obj' or the wrong directory in either
// the .gitattributes lines or the paths would be caught here.
func TestRunPreCommit_AllowsKnownBinariesViaRealGitattributes(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	// The exact lines this item added / relies on in the repo's .gitattributes.
	stage(".gitattributes", []byte(
		"tests/embed/*.bin -text\n"+
			"compiler/cmd/promise/crt/wasm32/*.o binary\n"))
	nul := []byte{'a', 0x00, 'b'}
	stage("compiler/cmd/promise/crt/wasm32/wasm_alloc.o", nul)
	stage("compiler/cmd/promise/crt/wasm32/wasm_math.o", nul)
	stage("tests/embed/data.bin", nul)
	if err := RunPreCommit(root); err != nil {
		t.Fatalf("expected the three known binaries to pass via their .gitattributes declarations, got: %v", err)
	}
}

// TestRunPreCommit_AllowsNulAfterScanWindow: the content gate mirrors git's
// buffer_is_binary heuristic, which scans only the first 8 KB. A blob whose
// first binaryScanLimit bytes are all text but that has a NUL past the window
// passes — even at an undeclared path. Exercises the head[:binaryScanLimit]
// truncation branch that the 3-byte blobs in the other tests never reach.
func TestRunPreCommit_AllowsNulAfterScanWindow(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	blob := append(bytes.Repeat([]byte{'a'}, binaryScanLimit), 0x00, 'a', 'b')
	stage("assets/late.dat", blob)
	if err := RunPreCommit(root); err != nil {
		t.Fatalf("expected no error — the NUL is past the %d-byte scan window: %v", binaryScanLimit, err)
	}
}

// TestRunPreCommit_RejectsNulAtScanWindowBoundary: the inverse boundary — a NUL
// at the last scanned byte (index binaryScanLimit-1) is inside the window and is
// rejected at an undeclared path. Together with the test above this pins the
// exact 8 KB cutoff to git's semantics (scan indices 0..binaryScanLimit-1).
func TestRunPreCommit_RejectsNulAtScanWindowBoundary(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	blob := append(bytes.Repeat([]byte{'a'}, binaryScanLimit-1), 0x00)
	stage("assets/edge.dat", blob)
	if err := RunPreCommit(root); err == nil {
		t.Fatalf("expected error — a NUL at the last byte of the %d-byte window is inside it", binaryScanLimit)
	}
}

// TestRunPreCommit_ReadsStagedBlobNotWorktree: the gate inspects the staged
// (index) blob, not the worktree copy. Staging binary then overwriting the
// worktree with text must still be rejected.
func TestRunPreCommit_ReadsStagedBlobNotWorktree(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	stage("assets/blob.dat", []byte{'a', 0x00, 'b'})
	// Overwrite the worktree copy with plain text — do NOT re-stage.
	if err := os.WriteFile(filepath.Join(root, "assets", "blob.dat"), []byte("now plain text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RunPreCommit(root); err == nil {
		t.Fatal("expected error — the gate must read the staged blob, not the worktree copy")
	}
}

// TestRunPreCommit_IgnoresBinaryWorktreeWhenStagedIsText: the inverse — a text
// blob is staged, then the worktree copy is overwritten with binary content
// that is never staged. The commit passes.
func TestRunPreCommit_IgnoresBinaryWorktreeWhenStagedIsText(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	stage("assets/blob.dat", []byte("plain text\n"))
	if err := os.WriteFile(filepath.Join(root, "assets", "blob.dat"), []byte{'a', 0x00, 'b'}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RunPreCommit(root); err != nil {
		t.Fatalf("expected no error — staged blob is text; the worktree binary is not staged: %v", err)
	}
}

// TestPathDeclaredBinary asserts the .gitattributes query: `binary` set and
// `-text` (text unset) both qualify; an undeclared path does not.
func TestPathDeclaredBinary(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	stage(".gitattributes", []byte("a/*.bin binary\nb/*.dat -text\n"))
	cases := []struct {
		path string
		want bool
	}{
		{"a/x.bin", true},  // binary attribute set
		{"b/x.dat", true},  // text unset (-text)
		{"c/x.txt", false}, // undeclared
	}
	for _, c := range cases {
		got, err := pathDeclaredBinary(root, c.path)
		if err != nil {
			t.Fatalf("pathDeclaredBinary(%q): %v", c.path, err)
		}
		if got != c.want {
			t.Errorf("pathDeclaredBinary(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// TestRunPreCommit_StagedDeletionIsQuiet stages a file deletion and asserts the
// commit passes AND does not leak git's `fatal:` diagnostic to stderr. The size
// probe (`cat-file -s :path`) necessarily fails for a deleted path — that
// failure is expected and handled by skipping the file — so it must stay quiet.
// Guards the RunOutputQuietIn choice against a regression to a stderr-connected
// probe, which would print `fatal:` on every commit that removes a file.
func TestRunPreCommit_StagedDeletionIsQuiet(t *testing.T) {
	root, _ := initGitRepoWithStager(t)

	// Stage the deletion of the seed file the helper committed.
	rm := exec.Command("git", "rm", "-q", "seed.txt")
	rm.Dir = root
	if out, err := rm.CombinedOutput(); err != nil {
		t.Fatalf("git rm seed.txt: %v\n%s", err, out)
	}

	// Capture os.Stderr around the check — RunOutputIn wires child stderr to the
	// current os.Stderr, so a regression would land in the pipe.
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	runErr := RunPreCommit(root)
	os.Stderr = orig
	w.Close()
	captured, _ := io.ReadAll(r)
	r.Close()

	if runErr != nil {
		t.Fatalf("staged deletion must pass the binary/size gate, got: %v", runErr)
	}
	if bytes.Contains(captured, []byte("fatal")) {
		t.Fatalf("staged deletion leaked a git diagnostic to stderr:\n%s", captured)
	}
}

// TestRunBytesIn_PreservesRawBytes locks RunBytesIn's contract: it returns the
// child's stdout byte-for-byte, untrimmed. This is why the binary-content gate
// uses it instead of RunOutputIn (whose TrimSpace would corrupt a blob). The
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

// initBareGitRepo creates an empty temp git repo (no commits) for tests that
// only exercise 'git var', which works without a commit history. Identity is
// driven entirely by the env vars each test sets via t.Setenv, which
// RunOutputIn's subprocess inherits since it does not override cmd.Env.
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

func TestCheckNoreplyIdentity_RejectsNonNoreplyAuthorEmail(t *testing.T) {
	root := initBareGitRepo(t)

	t.Setenv("GIT_AUTHOR_EMAIL", "personal@gmail.com")
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "1+test@users.noreply.github.com")
	t.Setenv("GIT_COMMITTER_NAME", "test")

	if err := checkNoreplyIdentity(root); err == nil {
		t.Fatal("expected error for non-noreply author email, got nil")
	}
}

func TestCheckNoreplyIdentity_RejectsNonNoreplyCommitterEmail(t *testing.T) {
	root := initBareGitRepo(t)

	t.Setenv("GIT_AUTHOR_EMAIL", "1+test@users.noreply.github.com")
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "personal@gmail.com")
	t.Setenv("GIT_COMMITTER_NAME", "test")

	if err := checkNoreplyIdentity(root); err == nil {
		t.Fatal("expected error for non-noreply committer email, got nil")
	}
}

func TestCheckNoreplyIdentity_AllowsNoreplyEmails(t *testing.T) {
	root := initBareGitRepo(t)

	t.Setenv("GIT_AUTHOR_EMAIL", "1+test@users.noreply.github.com")
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "1+test@users.noreply.github.com")
	t.Setenv("GIT_COMMITTER_NAME", "test")

	if err := checkNoreplyIdentity(root); err != nil {
		t.Fatalf("expected no error for noreply emails, got: %v", err)
	}
}

func TestIdentEmail(t *testing.T) {
	cases := []struct {
		ident string
		want  string
	}{
		{"Test User <1+test@users.noreply.github.com> 1700000000 +0000", "1+test@users.noreply.github.com"},
		{"Test User <> 1700000000 +0000", ""},
		{"no angle brackets here", ""},
	}
	for _, c := range cases {
		if got := identEmail(c.ident); got != c.want {
			t.Errorf("identEmail(%q) = %q, want %q", c.ident, got, c.want)
		}
	}
}

func TestRunPreCommit_RejectsLogFile(t *testing.T) {
	root := initGitRepoWithStagedFile(t, "Ctmpverify_t0925.log")
	if err := RunPreCommit(root); err == nil {
		t.Fatal("expected error for staged .log file, got nil")
	}
}

func TestRunPreCommit_RejectsLogFileInSubdir(t *testing.T) {
	root := initGitRepoWithStagedFile(t, "logs/run.log")
	if err := RunPreCommit(root); err == nil {
		t.Fatal("expected error for staged .log file in subdir, got nil")
	}
}

func TestRunPreCommit_RejectsNonASCIIFilename(t *testing.T) {
	root := initGitRepoWithStagedFile(t, "résumé.txt")
	if err := RunPreCommit(root); err == nil {
		t.Fatal("expected error for non-ASCII filename, got nil")
	}
}

func TestRunPreCommit_AllowsNormalFile(t *testing.T) {
	root := initGitRepoWithStagedFile(t, "src/main.go")
	if err := RunPreCommit(root); err != nil {
		t.Fatalf("expected no error for normal file, got: %v", err)
	}
}

func TestRunPreCommit_RejectsUnformattedGo(t *testing.T) {
	root := initGitRepoWithStagedFile(t, "seed2.txt")
	// gofmt-violating Go file under compiler/ (bad indentation + spacing).
	bad := "package main\nfunc  main(){\nx:=1\n_=x}\n"
	full := filepath.Join(root, "compiler", "internal", "bad.go")
	os.MkdirAll(filepath.Dir(full), 0o755)
	os.WriteFile(full, []byte(bad), 0o644)
	if err := RunPreCommit(root); err == nil {
		t.Fatal("expected error for unformatted Go file, got nil")
	}
}

func TestRunPreCommit_AllowsFormattedGo(t *testing.T) {
	root := initGitRepoWithStagedFile(t, "seed2.txt")
	good := "package main\n\nfunc main() {\n\tx := 1\n\t_ = x\n}\n"
	full := filepath.Join(root, "compiler", "internal", "good.go")
	os.MkdirAll(filepath.Dir(full), 0o755)
	os.WriteFile(full, []byte(good), 0o644)
	if err := RunPreCommit(root); err != nil {
		t.Fatalf("expected no error for formatted Go file, got: %v", err)
	}
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

func TestIsASCII(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"plain.txt", true},
		{"with space.txt", true},
		{"résumé.txt", false},
		{"café", false},
		{"", true},
	}
	for _, c := range cases {
		if got := isASCII(c.s); got != c.want {
			t.Errorf("isASCII(%q) = %v, want %v", c.s, got, c.want)
		}
	}
}

func TestValidateBaselinesDiff_NoRegression(t *testing.T) {
	head := Baselines{
		"linux-amd64": {
			"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"},
			"host_leak_count": {Value: fp(0), Direction: "down", Updated: "2026-04-06"},
		},
	}
	staged := Baselines{
		"linux-amd64": {
			"host_test_count": {Value: fp(110), Direction: "up", Updated: "2026-04-11"},
			"host_leak_count": {Value: fp(0), Direction: "down", Updated: "2026-04-06"},
		},
	}
	root := setupGitRepo(t, head, staged)

	err := validateBaselinesDiff(root)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestValidateBaselinesDiff_RegressionBlocked(t *testing.T) {
	head := Baselines{
		"linux-amd64": {
			"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"},
		},
	}
	staged := Baselines{
		"linux-amd64": {
			"host_test_count": {Value: fp(90), Direction: "up", Updated: "2026-04-11"},
		},
	}
	root := setupGitRepo(t, head, staged)

	err := validateBaselinesDiff(root)
	if err == nil {
		t.Fatal("expected regression error, got nil")
	}
}

func TestValidateBaselinesDiff_InformationalSkipped(t *testing.T) {
	// Informational metrics (no Direction) in HEAD should not block removal.
	head := Baselines{
		"linux-amd64": {
			"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"},
			"binary_size":     {Type: "informational"},
		},
	}
	staged := Baselines{
		"linux-amd64": {
			"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"},
			// binary_size removed — should be allowed since it's informational
		},
	}
	root := setupGitRepo(t, head, staged)

	err := validateBaselinesDiff(root)
	if err != nil {
		t.Fatalf("expected nil (informational removal allowed), got: %v", err)
	}
}

func TestValidateBaselinesDiff_PendingSkipped(t *testing.T) {
	// Pending metrics (Direction set but Value nil) in HEAD should not block.
	head := Baselines{
		"linux-amd64": {
			"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"},
			"coverage":        {Direction: "up"}, // Pending
		},
	}
	staged := Baselines{
		"linux-amd64": {
			"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"},
			"coverage":        {Value: fp(85.0), Direction: "up", Updated: "2026-04-11"},
		},
	}
	root := setupGitRepo(t, head, staged)

	err := validateBaselinesDiff(root)
	if err != nil {
		t.Fatalf("expected nil (pending populated), got: %v", err)
	}
}

func TestValidateBaselinesDiff_PlatformRemoved(t *testing.T) {
	head := Baselines{
		"linux-amd64": {
			"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"},
		},
	}
	staged := Baselines{
		// linux-amd64 removed entirely
	}
	root := setupGitRepo(t, head, staged)

	err := validateBaselinesDiff(root)
	if err == nil {
		t.Fatal("expected error for platform removal, got nil")
	}
}

func TestValidateBaselinesDiff_EnforcedMetricRemoved(t *testing.T) {
	head := Baselines{
		"linux-amd64": {
			"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"},
			"host_leak_count": {Value: fp(0), Direction: "down", Updated: "2026-04-06"},
		},
	}
	staged := Baselines{
		"linux-amd64": {
			"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"},
			// host_leak_count removed — should be blocked since it's enforced
		},
	}
	root := setupGitRepo(t, head, staged)

	err := validateBaselinesDiff(root)
	if err == nil {
		t.Fatal("expected error for enforced metric removal, got nil")
	}
}

func TestValidateBaselinesDiff_StagedValueNilTreatedAsZero(t *testing.T) {
	// If staged has a metric with nil Value but HEAD has it enforced,
	// the staged value should be treated as 0 for ratchet check.
	head := Baselines{
		"linux-amd64": {
			"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"},
		},
	}
	staged := Baselines{
		"linux-amd64": {
			"host_test_count": {Direction: "up"}, // Value nil → treated as 0 → regression
		},
	}
	root := setupGitRepo(t, head, staged)

	err := validateBaselinesDiff(root)
	if err == nil {
		t.Fatal("expected regression (nil staged value treated as 0 < 100), got nil")
	}
}

// TestRunPreCommit_RejectsDanglingMarkdownLink proves CheckDocs is actually
// wired into the hook. The doc checks are only worth anything if a bad commit
// is stopped at commit time — that is the whole point of mechanizing the T1675
// sweep instead of relying on the next manual pass.
func TestRunPreCommit_RejectsDanglingMarkdownLink(t *testing.T) {
	root := initGitRepoWithStagedFile(t, "docs/guide.md")
	os.WriteFile(filepath.Join(root, "docs", "guide.md"),
		[]byte("see [tags](tracker-tags.md)\n"), 0o644)
	err := RunPreCommit(root)
	if err == nil {
		t.Fatal("expected the hook to reject a dangling markdown link")
	}
	if !strings.Contains(err.Error(), "tracker-tags.md") {
		t.Fatalf("hook error should name the dangling target, got: %v", err)
	}
}

// TestRunPreCommit_AllowsCoherentDocs is the companion: a doc tree that
// satisfies the checks must pass through the hook untouched, so the gate
// cannot be "always red" and get disabled.
func TestRunPreCommit_AllowsCoherentDocs(t *testing.T) {
	root := initGitRepoWithStagedFile(t, "docs/index.md")
	os.WriteFile(filepath.Join(root, "docs", "index.md"),
		[]byte("- [guide](guide.md)\n"), 0o644)
	os.WriteFile(filepath.Join(root, "docs", "guide.md"), []byte("# Guide\n"), 0o644)
	cmd := exec.Command("git", "add", "docs/guide.md")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	if err := RunPreCommit(root); err != nil {
		t.Fatalf("a coherent doc tree must pass the hook, got: %v", err)
	}
}

// TestRunPreCommit_DocChecksRunBeforeStagedFileScan pins the ordering the
// wiring depends on: the staged-file scan returns early when nothing is
// staged, so a doc problem already committed to the tree would go unreported
// if CheckDocs ran after it.
func TestRunPreCommit_DocChecksRunBeforeStagedFileScan(t *testing.T) {
	root := initGitRepoWithStagedFile(t, "docs/guide.md")
	os.WriteFile(filepath.Join(root, "docs", "guide.md"),
		[]byte("see [tags](tracker-tags.md)\n"), 0o644)
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=1+test@users.noreply.github.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=1+test@users.noreply.github.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// Commit the bad doc so the staging area is empty but the tree still has
	// the dangling link.
	git("add", "docs/guide.md")
	git("commit", "-m", "add doc")

	if err := RunPreCommit(root); err == nil {
		t.Fatal("expected the doc checks to run even with an empty staging area")
	}
}
