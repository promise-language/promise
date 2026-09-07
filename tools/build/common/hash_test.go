package common

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeFile writes content at a repo-relative path, creating parent dirs.
func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// hashOf computes the worktree hash, failing the test on error.
func hashOf(t *testing.T, root string) string {
	t.Helper()
	h, err := WorktreeHash(root)
	if err != nil {
		t.Fatalf("WorktreeHash: %v", err)
	}
	return h
}

// worktreeRepo returns a git repo seeded with one source file.
func worktreeRepo(t *testing.T) string {
	t.Helper()
	root := initBareGitRepo(t)
	writeFile(t, root, "src/main.go", "package main\n")
	return root
}

func TestWorktreeHash_StableAcrossCalls(t *testing.T) {
	root := worktreeRepo(t)
	if a, b := hashOf(t, root), hashOf(t, root); a != b {
		t.Errorf("hash is not stable: %s vs %s", a, b)
	}
}

func TestWorktreeHash_ChangesOnContentEdit(t *testing.T) {
	root := worktreeRepo(t)
	before := hashOf(t, root)
	writeFile(t, root, "src/main.go", "package main // edited\n")
	if after := hashOf(t, root); after == before {
		t.Error("editing a file did not change the hash")
	}
}

func TestWorktreeHash_ChangesOnNewFile(t *testing.T) {
	root := worktreeRepo(t)
	before := hashOf(t, root)
	writeFile(t, root, "src/extra.go", "package main\n")
	if after := hashOf(t, root); after == before {
		t.Error("adding a non-ignored file did not change the hash")
	}
}

func TestWorktreeHash_ChangesOnDelete(t *testing.T) {
	root := worktreeRepo(t)
	before := hashOf(t, root)
	if err := os.Remove(filepath.Join(root, "src", "main.go")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if after := hashOf(t, root); after == before {
		t.Error("deleting a file did not change the hash")
	}
}

// A tracked file deleted from the worktree still appears in `git ls-files
// --cached`, so WorktreeHash must record its absence rather than fail.
func TestWorktreeHash_TrackedFileMissingFromWorktree(t *testing.T) {
	root := worktreeRepo(t)
	if err := RunIn(root, "git", "add", "-A"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	before := hashOf(t, root)
	if err := os.Remove(filepath.Join(root, "src", "main.go")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	after := hashOf(t, root)
	if after == before {
		t.Error("a staged deletion did not change the hash")
	}
}

// Identical bytes at two different paths must not hash alike — the path is
// part of the identity, not just the content.
func TestWorktreeHash_PathIsPartOfIdentity(t *testing.T) {
	a := initBareGitRepo(t)
	writeFile(t, a, "one.go", "same\n")
	b := initBareGitRepo(t)
	writeFile(t, b, "two.go", "same\n")
	if hashOf(t, a) == hashOf(t, b) {
		t.Error("same content at different paths produced the same hash")
	}
}

// The hash is a function of worktree content, not of the index, so staging is
// invisible to it. This is what lets "write a new test → verify → git add →
// commitgate" work without demanding a second verify.
func TestWorktreeHash_UnchangedByStaging(t *testing.T) {
	root := worktreeRepo(t)
	writeFile(t, root, "src/new_test.go", "package main\n")
	before := hashOf(t, root)
	if err := RunIn(root, "git", "add", "-A"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if after := hashOf(t, root); after != before {
		t.Errorf("staging changed the hash: %s -> %s", before, after)
	}
}

// .promise-home/ holds the sidecar the hash is recorded in — writing it must
// not change the identity it describes.
func TestWorktreeHash_ExcludesPromiseHome(t *testing.T) {
	root := worktreeRepo(t)
	before := hashOf(t, root)
	writeFile(t, root, ".promise-home/"+gateValuesFile, `{"values":{}}`)
	if after := hashOf(t, root); after != before {
		t.Error(".promise-home/ content changed the worktree hash")
	}
}

// baselines.json is the commit gate's own output — a passing run rewrites it,
// so it must not be part of the tree identity that run validated.
func TestWorktreeHash_ExcludesBaselines(t *testing.T) {
	root := worktreeRepo(t)
	writeFile(t, root, baselinesFile, "{}\n")
	before := hashOf(t, root)
	writeFile(t, root, baselinesFile, `{"linux-amd64":{}}`+"\n")
	if after := hashOf(t, root); after != before {
		t.Error("rewriting baselines.json changed the worktree hash")
	}
}

func TestWorktreeHash_ExcludesIgnoredFiles(t *testing.T) {
	root := worktreeRepo(t)
	writeFile(t, root, ".gitignore", "build/\n")
	before := hashOf(t, root)
	writeFile(t, root, "build/artifact.o", "binary-ish\n")
	if after := hashOf(t, root); after != before {
		t.Error("an ignored file changed the worktree hash")
	}
}

// An unmerged path is listed once per merge stage by `git ls-files --cached`.
// The identity is worktree content, so the file must count once however many
// stages the index is carrying — this pins the dedupe and "the index is out"
// with the same assertion.
func TestWorktreeHash_UnmergedPathCountsOnce(t *testing.T) {
	root := worktreeRepo(t)
	want := hashOf(t, root)

	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	blob := git("hash-object", "-w", "src/main.go")

	// Stage the same blob at all three merge stages, as a conflict would.
	var entries strings.Builder
	for stage := 1; stage <= 3; stage++ {
		fmt.Fprintf(&entries, "100644 %s %d\tsrc/main.go\n", blob, stage)
	}
	cmd := exec.Command("git", "update-index", "--index-info")
	cmd.Dir = root
	cmd.Stdin = strings.NewReader(entries.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("update-index: %v\n%s", err, out)
	}
	if listed := strings.Count(git("ls-files", "--cached"), "src/main.go"); listed != 3 {
		t.Fatalf("expected git to list the unmerged path 3 times, got %d", listed)
	}

	if got := hashOf(t, root); got != want {
		t.Errorf("merge stages changed the worktree identity: %s -> %s", want, got)
	}
}

func TestWorktreeHash_NotAGitRepo(t *testing.T) {
	// Clear TMPDIR so t.TempDir() uses /tmp rather than .promise-home/tmp.
	// bin/verify points TMPDIR inside the repo (SetupLocalCache), which would
	// put this "repo" inside the real work tree and make git ls-files succeed
	// — the same hazard as TestGitSHA_NotARepo.
	t.Setenv("TMPDIR", "")
	if _, err := WorktreeHash(t.TempDir()); err == nil {
		t.Fatal("expected an error outside a git repository, got nil")
	}
}

// A rename is a content-preserving edit that must still change the identity:
// the tree the gate values described no longer exists. The bytes are the same,
// only the path moved.
func TestWorktreeHash_RenameChangesIdentity(t *testing.T) {
	root := worktreeRepo(t)
	before := hashOf(t, root)
	if err := os.Rename(filepath.Join(root, "src", "main.go"),
		filepath.Join(root, "src", "renamed.go")); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if after := hashOf(t, root); after == before {
		t.Error("renaming a file did not change the hash")
	}
}

// Each file contributes its byte length before its bytes, which is what keeps
// the digest stream unambiguous. These two trees are the collision that framing
// on the path alone would produce: hashing "<path>\n" + content over each, the
// left tree feeds "a\n" + "" + "b\n" + "X" and the right feeds "a\n" + "b\nX"
// — the same bytes for two genuinely different trees. With the length in
// between they diverge, so a file cannot be split or merged behind the gate's
// back.
func TestWorktreeHash_LengthFramingPreventsCollision(t *testing.T) {
	twoFiles := initBareGitRepo(t)
	writeFile(t, twoFiles, "a", "")
	writeFile(t, twoFiles, "b", "X")

	oneFile := initBareGitRepo(t)
	writeFile(t, oneFile, "a", "b\nX")

	if hashOf(t, twoFiles) == hashOf(t, oneFile) {
		t.Error("trees that differ only in where a file boundary falls hashed alike")
	}
}

// git lists an untracked nested repository as the *directory*, not its files
// (it does not recurse). Reading that path fails, which must be recorded
// rather than fatal — a gate that crashed here would be worse than the clock
// it replaced.
func TestWorktreeHash_UntrackedNestedRepository(t *testing.T) {
	root := worktreeRepo(t)
	before := hashOf(t, root)

	nested := filepath.Join(root, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	if err := RunIn(nested, "git", "init"); err != nil {
		t.Fatalf("git init nested: %v", err)
	}
	writeFile(t, root, "nested/inner.go", "package nested\n")

	listed, err := RunBytesIn(root, "git", "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		t.Fatalf("ls-files: %v", err)
	}
	if !strings.Contains(string(listed), "nested/\x00") {
		t.Fatalf("expected git to list the nested repo as a directory, got %q", listed)
	}

	after := hashOf(t, root)
	if after == before {
		t.Error("an untracked nested repository did not change the hash")
	}
	// Stable while it is there — the unreadable path must hash deterministically.
	if again := hashOf(t, root); again != after {
		t.Errorf("unreadable path is not hashed deterministically: %s vs %s", after, again)
	}

	// Its *contents* are correctly invisible: git does not list them, and
	// nothing inside an unadded nested repo can reach a commit of this one, so
	// they are not part of what the gate vouches for.
	writeFile(t, root, "nested/inner.go", "package nested // edited\n")
	if got := hashOf(t, root); got != after {
		t.Errorf("editing inside a nested repository changed the hash: %s -> %s", after, got)
	}
}

// Paths come from `git ls-files -z` through RunBytesIn precisely so that odd
// names survive: -z emits raw bytes with no quoting, and RunBytesIn does not
// trim. Reading the list through RunOutputIn instead would strip the leading
// space off the first path, silently turning that file into an unreadable one
// whose edits no longer move the hash — which is what this asserts.
func TestWorktreeHash_OddFileNames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("leading spaces and newlines are not portable in Windows file names")
	}
	root := initBareGitRepo(t)
	// Sorts first, so it is the path a TrimSpace on the whole list would eat.
	const leading = " leading space.go"
	const newline = "with\nnewline.txt"
	writeFile(t, root, leading, "original\n")
	writeFile(t, root, newline, "original\n")
	before := hashOf(t, root)

	writeFile(t, root, leading, "edited\n")
	afterLeading := hashOf(t, root)
	if afterLeading == before {
		t.Error("editing a file whose name starts with a space did not change the hash")
	}

	writeFile(t, root, newline, "edited\n")
	if hashOf(t, root) == afterLeading {
		t.Error("editing a file whose name contains a newline did not change the hash")
	}
}

// The exclusion list carries two shapes with different matching rules, and
// getting either wrong silently drops real source from the identity: a
// directory prefix must not match a longer sibling directory, and an exact
// path must not match anything built on top of it.
func TestGateHashExcluded(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{".promise-home/gate-values.json", true},
		{".promise-home/cache/llvm/x", true},
		{".promise-home", false},             // the directory entry itself, not a file under it
		{".promise-home-backup/a.go", false}, // sibling that merely shares the prefix
		{"tools/gates/baselines.json", true},
		{"tools/gates/baselines.json.bak", false},
		{"tools/gates/baselines.jsonx", false},
		{"a/tools/gates/baselines.json", false}, // exclusions are anchored at the root
		{"tools/gates/gate.go", false},
		{"src/main.go", false},
	}
	for _, c := range cases {
		if got := gateHashExcluded(c.path); got != c.want {
			t.Errorf("gateHashExcluded(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// --- ToolsSourceHash ------------------------------------------------------

// toolsRepo returns a root with one Go source file under tools/build/.
func toolsRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, "tools/build/common/thing.go", "package common\n")
	return root
}

func TestToolsSourceHash_StableAcrossCalls(t *testing.T) {
	root := toolsRepo(t)
	a, err := ToolsSourceHash(root)
	if err != nil {
		t.Fatalf("ToolsSourceHash: %v", err)
	}
	b, err := ToolsSourceHash(root)
	if err != nil {
		t.Fatalf("ToolsSourceHash: %v", err)
	}
	if a != b {
		t.Errorf("hash is not stable: %s vs %s", a, b)
	}
}

func TestToolsSourceHash_ChangesOnSourceEdit(t *testing.T) {
	root := toolsRepo(t)
	before, _ := ToolsSourceHash(root)
	writeFile(t, root, "tools/build/common/thing.go", "package common // edited\n")
	after, err := ToolsSourceHash(root)
	if err != nil {
		t.Fatalf("ToolsSourceHash: %v", err)
	}
	if after == before {
		t.Error("editing a .go file did not change the tools hash")
	}
}

// The staleness signal covers what the tools are built from: Go sources and
// the module files that pin their dependencies. Anything else under
// tools/build/ — a README, a testdata fixture — must not force a ./make.
func TestToolsSourceHash_CoversSourceAndModuleFilesOnly(t *testing.T) {
	root := toolsRepo(t)
	base, _ := ToolsSourceHash(root)

	writeFile(t, root, "tools/build/README.md", "docs\n")
	writeFile(t, root, "tools/build/common/testdata/fixture.json", "{}\n")
	ignored, err := ToolsSourceHash(root)
	if err != nil {
		t.Fatalf("ToolsSourceHash: %v", err)
	}
	if ignored != base {
		t.Error("a non-source file under tools/build/ changed the tools hash")
	}

	for _, name := range []string{"go.mod", "go.sum"} {
		writeFile(t, root, "tools/build/"+name, "module x\n")
		got, err := ToolsSourceHash(root)
		if err != nil {
			t.Fatalf("ToolsSourceHash: %v", err)
		}
		if got == base {
			t.Errorf("adding %s did not change the tools hash", name)
		}
		base = got
	}
}

// Sources outside tools/build/ are a different binary's problem — the compiler
// tree changing must not report the build tools as stale.
func TestToolsSourceHash_IgnoresSourceOutsideToolsBuild(t *testing.T) {
	root := toolsRepo(t)
	before, _ := ToolsSourceHash(root)
	writeFile(t, root, "compiler/internal/codegen/expr.go", "package codegen\n")
	after, err := ToolsSourceHash(root)
	if err != nil {
		t.Fatalf("ToolsSourceHash: %v", err)
	}
	if after != before {
		t.Error("a .go file outside tools/build/ changed the tools hash")
	}
}

func TestToolsSourceHash_MissingToolsDir(t *testing.T) {
	if _, err := ToolsSourceHash(t.TempDir()); err == nil {
		t.Fatal("expected an error with no tools/build directory, got nil")
	}
}

// A source file that cannot be read makes the staleness signal unknowable, so
// ToolsSourceHash must fail rather than hash the tree as if the file were
// absent — a silent skip would let a stale binary claim to be current.
func TestToolsSourceHash_UnreadableSource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod does not deny reads on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	root := toolsRepo(t)
	path := filepath.Join(root, "tools", "build", "common", "thing.go")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(path, 0o644)

	if _, err := ToolsSourceHash(root); err == nil {
		t.Fatal("expected an error for an unreadable source file, got nil")
	}
}
