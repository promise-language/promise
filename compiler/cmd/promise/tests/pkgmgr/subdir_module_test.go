package pkgmgr

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
	"github.com/promise-language/promise/compiler/internal/module"
)

// TestBuildSubdirRemoteModules is the T1524 end-to-end gate: a project addresses
// two modules that live in subdirectories of one repo (which has no root
// manifest), both link into one binary, and each gets its own IR prefix.
func TestBuildSubdirRemoteModules(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping build integration test in short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("local repo paths contain ':' which is invalid in Windows cache paths")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	cli := clitest.NewEnv(t)

	repo, commit := clitest.MakeSubdirRepo(t, map[string]string{
		"wire":  "proto/wire",
		"types": "proto/types",
	})

	dir := t.TempDir()
	manifest := "[module]\nname = \"app\"\nepoch = \"2026.0\"\nmain = \"main.pr\"\n\n" +
		"[require.wire]\nurl = \"" + repo + "\"\ncommit = \"" + commit + "\"\nsubdir = \"proto/wire\"\n\n" +
		"[require.types]\nurl = \"" + repo + "\"\ncommit = \"" + commit + "\"\nsubdir = \"proto/types\"\n"
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	// The import name is the [require.NAME] key — addressing is in the manifest,
	// the source just says `use wire;`.
	src := "use wire;\nuse types;\n\nmain!() {\n  print_line(\"{wire.greet()}-{types.greet()}\");\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.pr"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	outStr, err := cli.Promise(t, dir, "run", ".")
	out := []byte(outStr)
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "wire-types") {
		t.Fatalf("expected both subdir modules to link in, got:\n%s", out)
	}

	// Each module must carry its own IR prefix — without the subdir in the module
	// identity both would sanitize to the same prefix and collide.
	irStr, err := cli.Promise(t, dir, "emit-ir", ".")
	ir := []byte(irStr)
	if err != nil {
		t.Fatalf("emit-ir failed: %v\n%s", err, ir)
	}
	norm := module.NormalizeURL(repo)
	wirePrefix := module.SanitizeIRPrefix(module.GlobalIdentityForRemote(norm, "proto/wire"))
	typesPrefix := module.SanitizeIRPrefix(module.GlobalIdentityForRemote(norm, "proto/types"))
	if wirePrefix == typesPrefix {
		t.Fatal("subdir modules produced the same IR prefix")
	}
	for _, p := range []string{wirePrefix, typesPrefix} {
		if !strings.Contains(string(ir), "__mod_"+p) {
			t.Errorf("emitted IR is missing the %q module prefix", p)
		}
	}
}

// A [require.NAME] entry whose subdir names a directory with no promise.toml must
// fail with a message that points at the subdir.
func TestBuildSubdirRemoteModuleMissingManifest(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping build integration test in short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("local repo paths contain ':' which is invalid in Windows cache paths")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	cli := clitest.NewEnv(t)

	repo, commit := clitest.MakeSubdirRepo(t, map[string]string{"wire": "proto/wire"})

	dir := t.TempDir()
	manifest := "[module]\nname = \"app\"\nepoch = \"2026.0\"\nmain = \"main.pr\"\n\n" +
		"[require.wire]\nurl = \"" + repo + "\"\ncommit = \"" + commit + "\"\nsubdir = \"proto/nope\"\n"
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.pr"),
		[]byte("use wire;\n\nmain!() {\n  print_line(wire.greet());\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	outStr, _ := cli.Promise(t, dir, "build", ".")
	out := []byte(outStr)
	if !strings.Contains(string(out), "no promise.toml at") || !strings.Contains(string(out), "proto/nope") {
		t.Fatalf("expected an error naming the missing subdir, got:\n%s", out)
	}
}

// [replace] matches the bare repo URL, so one line redirects every subdir module
// in that repo to a local checkout — and a replaced module keeps the remote
// identity (same IR prefix), so the build cache does not split.
func TestSubdirRemoteModuleReplace(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping build integration test in short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("local repo paths contain ':' which is invalid in Windows cache paths")
	}
	cli := clitest.NewEnv(t)

	// A local checkout of the same repo layout — no git needed, [replace] wins
	// before any fetch.
	local := filepath.Join(t.TempDir(), "base")
	for name, sub := range map[string]string{"wire": "proto/wire", "types": "proto/types"} {
		d := filepath.Join(local, filepath.FromSlash(sub))
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "promise.toml"),
			[]byte("[module]\nname = \""+name+"\"\nepoch = \"2026.0\"\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, name+".pr"),
			[]byte("greet() string `public {\n  return \""+name+"\";\n}\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	const repoURL = "https://github.com/acme/base"
	dir := t.TempDir()
	manifest := "[module]\nname = \"app\"\nepoch = \"2026.0\"\nmain = \"main.pr\"\n\n" +
		"[require.wire]\nurl = \"" + repoURL + "\"\ncommit = \"deadbeef\"\nsubdir = \"proto/wire\"\n\n" +
		"[require.types]\nurl = \"" + repoURL + "\"\ncommit = \"deadbeef\"\nsubdir = \"proto/types\"\n\n" +
		"[replace]\n\"" + repoURL + "\" = \"" + local + "\"\n"
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.pr"),
		[]byte("use wire;\nuse types;\n\nmain!() {\n  print_line(\"{wire.greet()}-{types.greet()}\");\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	outStr, err := cli.Promise(t, dir, "run", ".")
	out := []byte(outStr)
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "wire-types") {
		t.Fatalf("expected both replaced subdir modules to link in, got:\n%s", out)
	}

	irStr, err := cli.Promise(t, dir, "emit-ir", ".")
	ir := []byte(irStr)
	if err != nil {
		t.Fatalf("emit-ir failed: %v\n%s", err, ir)
	}
	// Replaced modules keep the REMOTE identity, not the local path identity.
	wirePrefix := module.SanitizeIRPrefix(module.GlobalIdentityForRemote(module.NormalizeURL(repoURL), "proto/wire"))
	if !strings.Contains(string(ir), "__mod_"+wirePrefix) {
		t.Errorf("replaced module lost its remote identity (missing prefix %q)", wirePrefix)
	}
}

// T1611 regression: a remote module addressed by a [require.NAME] entry at the
// repo ROOT (no subdir) must build. Module functions are declared under the
// module's URL-derived IR prefix, so resolving `wire.greet` by assuming
// prefix == import name panicked in codegen.
func TestBuildNamedRequireRootModule(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping build integration test in short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("local repo paths contain ':' which is invalid in Windows cache paths")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	cli := clitest.NewEnv(t)

	// Empty subdir → the module's promise.toml lands at the repo root.
	repo, commit := clitest.MakeSubdirRepo(t, map[string]string{"wire": ""})

	dir := t.TempDir()
	manifest := "[module]\nname = \"app\"\nepoch = \"2026.0\"\nmain = \"main.pr\"\n\n" +
		"[require.wire]\nurl = \"" + repo + "\"\ncommit = \"" + commit + "\"\n"
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.pr"),
		[]byte("use wire;\n\nmain!() {\n  print_line(wire.greet());\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	outStr, err := cli.Promise(t, dir, "run", ".")
	out := []byte(outStr)
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "wire") {
		t.Fatalf("expected the named-require module to link in, got:\n%s", out)
	}
}

// T1611: an aliased catalog import (`use json as j;`) must still resolve to the
// catalog module's IR prefix, not the alias.
func TestBuildAliasedCatalogImport(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping build integration test in short mode")
	}
	cli := clitest.NewEnv(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"),
		[]byte("[module]\nname = \"app\"\nepoch = \"2026.0\"\nmain = \"main.pr\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	src := "use path as p;\n\nmain!() {\n  print_line(p.file_name(\"/a/b/c.txt\"));\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.pr"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	outStr, err := cli.Promise(t, dir, "run", ".")
	out := []byte(outStr)
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "c.txt") {
		t.Fatalf("expected aliased catalog call to work, got:\n%s", out)
	}
}

// T1611: two aliased catalog imports in different files must each resolve to
// their own IR prefix, not to their alias. (Sema makes aliases project-unique, so
// the ambiguous same-alias case cannot arise — see TestSameAliasTwoModulesRejected.)
func TestBuildTwoAliasedModulesAcrossFiles(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping build integration test in short mode")
	}
	cli := clitest.NewEnv(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"),
		[]byte("[module]\nname = \"app\"\nepoch = \"2026.0\"\nmain = \"main.pr\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.pr"),
		[]byte("use path as p;\n\nmain!() {\n  print_line(p.file_name(\"/a/b/c.txt\"));\n  print_line(\"{other()}\");\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "other.pr"),
		[]byte("use math as mm;\n\nother() int {\n  return mm.gcd(12, 18);\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	outStr, err := cli.Promise(t, dir, "run", ".")
	out := []byte(outStr)
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "c.txt") || !strings.Contains(string(out), "6") {
		t.Fatalf("expected both aliased modules to resolve, got:\n%s", out)
	}
}

// Imports are file-scoped (T1686): two files may bind the same alias to different
// modules, each resolving to its own module within its own file. Codegen resolves
// each qualified call site through the sema-recorded module object, so the
// import-name → IR prefix mapping stays unambiguous per file (supersedes T1611,
// which rejected this while imports were still module-scoped).
func TestSameAliasTwoModulesAllowed(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping build integration test in short mode")
	}
	cli := clitest.NewEnv(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"),
		[]byte("[module]\nname = \"app\"\nepoch = \"2026.0\"\nmain = \"main.pr\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.pr"),
		[]byte("use path as m;\n\nmain!() {\n  print_line(m.file_name(\"/a/b.txt\"));\n  print_line(other().to_string());\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "other.pr"),
		[]byte("use math as m;\n\nother() int `public {\n  return m.gcd(12, 18);\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	outStr, err := cli.Promise(t, dir, "run", ".")
	out := []byte(outStr)
	if err != nil {
		t.Fatalf("expected both aliased modules to resolve, run failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "b.txt") || !strings.Contains(string(out), "6") {
		t.Fatalf("expected path.file_name and math.gcd to both resolve, got:\n%s", out)
	}
}

// T1524: `promise package add --subdir` writes only [require.NAME] sections, so
// `promise package remove` must accept an import name — and a URL that backs
// several subdir modules must take all of them out.
func TestBuildSubdirModulesAtDifferentCommits(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping build integration test in short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("local repo paths contain ':' which is invalid in Windows cache paths")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	cli := clitest.NewEnv(t)

	work := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(work, 0755); err != nil {
		t.Fatal(err)
	}
	clitest.GitRun(t, work, "init", "--initial-branch=main")
	clitest.GitRun(t, work, "config", "user.email", "test@test.com")
	clitest.GitRun(t, work, "config", "user.name", "Test")

	writeSub := func(sub, name, body string) {
		t.Helper()
		dir := filepath.Join(work, filepath.FromSlash(sub))
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "promise.toml"),
			[]byte("[module]\nname = \""+name+"\"\nepoch = \"2026.0\"\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".pr"),
			[]byte("greet() string `public {\n  return \""+body+"\";\n}\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Commit 1: both modules say "v1".
	writeSub("proto/wire", "wire", "wire-v1")
	writeSub("proto/types", "types", "types-v1")
	clitest.GitRun(t, work, "add", ".")
	clitest.GitRun(t, work, "commit", "-m", "v1")
	commit1 := clitest.GitRun(t, work, "rev-parse", "HEAD")

	// Commit 2: only `types` moves forward.
	writeSub("proto/types", "types", "types-v2")
	clitest.GitRun(t, work, "add", ".")
	clitest.GitRun(t, work, "commit", "-m", "v2")
	commit2 := clitest.GitRun(t, work, "rev-parse", "HEAD")

	repo := filepath.Join(t.TempDir(), "base.git")
	clitest.GitRun(t, "", "clone", "--bare", "--quiet", work, repo)

	dir := t.TempDir()
	manifest := "[module]\nname = \"app\"\nepoch = \"2026.0\"\nmain = \"main.pr\"\n\n" +
		"[require.wire]\nurl = \"" + repo + "\"\ncommit = \"" + commit1 + "\"\nsubdir = \"proto/wire\"\n\n" +
		"[require.types]\nurl = \"" + repo + "\"\ncommit = \"" + commit2 + "\"\nsubdir = \"proto/types\"\n"
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.pr"),
		[]byte("use wire;\nuse types;\n\nmain!() {\n  print_line(\"{wire.greet()}/{types.greet()}\");\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	outStr, err := cli.Promise(t, dir, "run", ".")
	out := []byte(outStr)
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "wire-v1/types-v2") {
		t.Fatalf("each subdir module must build from its own pinned commit, got:\n%s", out)
	}
}

// `promise package update` on a subdir entry must re-resolve the *addressed
// module* (the §9.9 gate compiles and tests proto/wire, not the repo root) and
// rewrite only the commit — the subdir line has to survive, or the entry would
// silently re-point at a root that has no manifest (T1524).
