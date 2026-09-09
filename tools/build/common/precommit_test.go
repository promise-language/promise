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

// initBareGitRepo creates an empty temp git repo (no commits) — enough for the
// checks in this package that never look at HEAD: 'git var' and 'git ls-files'
// (which reads the index and the worktree). Contrast initGitRepo in
// version_test.go, which seeds a commit because the provenance tests need a
// SHA. For the 'git var' tests, identity is driven entirely by the env vars
// each test sets via t.Setenv, which RunOutputIn's subprocess inherits since it
// does not override cmd.Env.
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

// TestRunPreCommit_RejectsSleepInTestFile is the full integration path: a
// staged test .pr file that calls sleep() causes RunPreCommit to return an
// error naming the file, proving the guard is wired into the hook.
func TestRunPreCommit_RejectsSleepInTestFile(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	stage("tests/new/sync_test.pr", []byte("test_x() `test { sleep(Duration.from_millis(10)); }\n"))
	err := RunPreCommit(root)
	if err == nil {
		t.Fatal("expected RunPreCommit to reject sleep() in a test file, got nil")
	}
	if !strings.Contains(err.Error(), "tests/new/sync_test.pr") {
		t.Errorf("error should name the violating file, got: %v", err)
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
	if !strings.Contains(err.Error(), "list tracked Promise files") {
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
	if !strings.Contains(err.Error(), "list tracked Promise files") {
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

// The guard is only a guard if RunPreCommit actually calls it.
func TestRunPreCommit_RejectsFixedTempPathInTestFile(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	stage("tests/new/scratch_test.pr",
		[]byte("test_x() `test { string p = os.temp_dir + \"/pr_fixed\"; }\n"))
	err := RunPreCommit(root)
	if err == nil {
		t.Fatal("expected RunPreCommit to reject a fixed temp path in a test file, got nil")
	}
	if !strings.Contains(err.Error(), "tests/new/scratch_test.pr") {
		t.Errorf("error should name the violating file, got: %v", err)
	}
}
