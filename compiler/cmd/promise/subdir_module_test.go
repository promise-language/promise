package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/module"
)

// makeSubdirRepo builds a bare git repo with NO promise.toml at its root and one
// Promise module per entry in mods (name → repo-relative subdir) — the T1524
// shape: a repo that is not itself Promise-primary but contains Promise modules.
// Returns the bare repo path and the commit SHA.
func makeSubdirRepo(t *testing.T, mods map[string]string) (bareRepo, commit string) {
	t.Helper()

	work := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(work, 0755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "init", "--initial-branch=main")
	gitRun(t, work, "config", "user.email", "test@test.com")
	gitRun(t, work, "config", "user.name", "Test")

	// Root marker for a non-Promise-primary repo — deliberately no promise.toml.
	if err := os.WriteFile(filepath.Join(work, "go.mod"), []byte("module example.com/base\n"), 0644); err != nil {
		t.Fatal(err)
	}

	for name, sub := range mods {
		dir := filepath.Join(work, filepath.FromSlash(sub))
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "promise.toml"),
			[]byte("[module]\nname = \""+name+"\"\nepoch = \"2026.0\"\n"), 0644); err != nil {
			t.Fatal(err)
		}
		src := "greet() string `public {\n  return \"" + name + "\";\n}\n"
		if err := os.WriteFile(filepath.Join(dir, name+".pr"), []byte(src), 0644); err != nil {
			t.Fatal(err)
		}
	}

	gitRun(t, work, "add", ".")
	gitRun(t, work, "commit", "-m", "initial")
	commit = gitRun(t, work, "rev-parse", "HEAD")

	bareRepo = filepath.Join(t.TempDir(), "base.git")
	gitRun(t, "", "clone", "--bare", "--quiet", work, bareRepo)
	return bareRepo, commit
}

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
	cli := newCLIEnv(t)

	repo, commit := makeSubdirRepo(t, map[string]string{
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

	outStr, err := cli.promise(t, dir, "run", ".")
	out := []byte(outStr)
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "wire-types") {
		t.Fatalf("expected both subdir modules to link in, got:\n%s", out)
	}

	// Each module must carry its own IR prefix — without the subdir in the module
	// identity both would sanitize to the same prefix and collide.
	irStr, err := cli.promise(t, dir, "emit-ir", ".")
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
	cli := newCLIEnv(t)

	repo, commit := makeSubdirRepo(t, map[string]string{"wire": "proto/wire"})

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

	outStr, _ := cli.promise(t, dir, "build", ".")
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
	cli := newCLIEnv(t)

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

	outStr, err := cli.promise(t, dir, "run", ".")
	out := []byte(outStr)
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "wire-types") {
		t.Fatalf("expected both replaced subdir modules to link in, got:\n%s", out)
	}

	irStr, err := cli.promise(t, dir, "emit-ir", ".")
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
	cli := newCLIEnv(t)

	// Empty subdir → the module's promise.toml lands at the repo root.
	repo, commit := makeSubdirRepo(t, map[string]string{"wire": ""})

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

	outStr, err := cli.promise(t, dir, "run", ".")
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
	cli := newCLIEnv(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"),
		[]byte("[module]\nname = \"app\"\nepoch = \"2026.0\"\nmain = \"main.pr\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	src := "use path as p;\n\nmain!() {\n  print_line(p.file_name(\"/a/b/c.txt\"));\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.pr"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	outStr, err := cli.promise(t, dir, "run", ".")
	out := []byte(outStr)
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "c.txt") {
		t.Fatalf("expected aliased catalog call to work, got:\n%s", out)
	}
}

func TestParseAddFlags(t *testing.T) {
	t.Parallel()
	cases := []struct {
		args       []string
		wantPos    []string
		wantSubdir string
		wantName   string
	}{
		{[]string{"url"}, []string{"url"}, "", ""},
		{[]string{"--subdir", "proto/wire", "url"}, []string{"url"}, "proto/wire", ""},
		{[]string{"url", "--subdir=proto/wire"}, []string{"url"}, "proto/wire", ""},
		{[]string{"--name", "w", "--subdir", "a/b", "url", "v1"}, []string{"url", "v1"}, "a/b", "w"},
		{[]string{"--name=w", "url"}, []string{"url"}, "", "w"},
	}
	for _, tc := range cases {
		pos, subdir, name, err := parseAddFlags(tc.args)
		if err != nil {
			t.Errorf("parseAddFlags(%v): %v", tc.args, err)
			continue
		}
		if strings.Join(pos, ",") != strings.Join(tc.wantPos, ",") || subdir != tc.wantSubdir || name != tc.wantName {
			t.Errorf("parseAddFlags(%v) = %v, %q, %q; want %v, %q, %q",
				tc.args, pos, subdir, name, tc.wantPos, tc.wantSubdir, tc.wantName)
		}
	}

	for _, args := range [][]string{{"--subdir"}, {"url", "--name"}} {
		if _, _, _, err := parseAddFlags(args); err == nil {
			t.Errorf("parseAddFlags(%v): expected a missing-value error", args)
		}
	}
}

// T1524: `promise package add --subdir` writes a [require.NAME] section carrying
// url, commit and subdir, and verifies the addressed module (not the repo root).
func TestAddWithSubdir(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping verify integration test in short mode")
	}
	t.Parallel()
	cli := newCLIEnv(t)
	epoch := compilerEpochForTest(t)

	bareDir := filepath.ToSlash(shortRepoDir(t))
	workDir := shortRepoDir(t)
	projDir := t.TempDir()

	run := func(dir, name string, args ...string) { cli.run(t, dir, name, args...) }

	run(bareDir, "git", "init", "--bare", ".")
	run(workDir, "git", "clone", bareDir, ".")
	// The module lives in proto/wire; the repo root has no promise.toml.
	sub := filepath.Join(workDir, "proto", "wire")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	writeMod(t, sub, "wire", true)
	run(workDir, "git", "add", ".")
	run(workDir, "git", "commit", "-m", "first")
	run(workDir, "git", "tag", "v1.0")
	run(workDir, "git", "push", "origin", "HEAD", "--tags")

	if err := os.WriteFile(filepath.Join(projDir, "promise.toml"),
		[]byte("[module]\nname = \"proj\"\nepoch = \""+epoch+"\"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	out := cli.promiseOK(t, projDir, "package", "add", "--subdir", "proto/wire", bareDir, "v1.0")
	if !strings.Contains(out, "use wire;") {
		t.Errorf("expected the import hint in output, got: %s", out)
	}

	cfg, err := module.ParseConfig(filepath.Join(projDir, "promise.toml"))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	e := cfg.NamedRequire["wire"]
	if e == nil {
		t.Fatalf("expected a [require.wire] entry, got %+v", cfg.NamedRequire)
	}
	if e.Subdir != "proto/wire" {
		t.Errorf("subdir = %q, want %q", e.Subdir, "proto/wire")
	}
	if e.Commit == "" {
		t.Error("expected a pinned commit")
	}
	if len(cfg.Require) != 0 {
		t.Errorf("--subdir must not write a flat [require] entry, got %v", cfg.Require)
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
	cli := newCLIEnv(t)

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

	outStr, err := cli.promise(t, dir, "run", ".")
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
	cli := newCLIEnv(t)

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

	outStr, err := cli.promise(t, dir, "run", ".")
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
func TestPackageRemoveNamedEntries(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	manifest := `[module]
name = "app"
epoch = "2026.0"

[require.wire]
url = "https://github.com/acme/base"
commit = "abc123"
subdir = "proto/wire"

[require.types]
url = "https://github.com/acme/base"
commit = "abc123"
subdir = "proto/types"

[require.other]
url = "https://github.com/acme/other"
commit = "def456"
`
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}

	// By import name — removes just that entry.
	out := captureStdout(t, func() { captureStderr(func() { runPackageRemove([]string{"wire"}) }) })
	if !strings.Contains(out, "[require.wire]") {
		t.Errorf("expected a removal message naming the entry, got: %s", out)
	}
	cfg, err := module.ParseConfig(filepath.Join(dir, "promise.toml"))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if _, ok := cfg.NamedRequire["wire"]; ok {
		t.Error("[require.wire] survived removal by name")
	}
	if _, ok := cfg.NamedRequire["types"]; !ok {
		t.Error("removing 'wire' by name also removed its sibling")
	}

	// By URL — removes every named entry addressed in that repo.
	captureStdout(t, func() {
		captureStderr(func() { runPackageRemove([]string{"https://github.com/acme/base"}) })
	})
	cfg, err = module.ParseConfig(filepath.Join(dir, "promise.toml"))
	if err != nil {
		t.Fatalf("ParseConfig after URL remove: %v", err)
	}
	if _, ok := cfg.NamedRequire["types"]; ok {
		t.Error("[require.types] survived removal by repo URL")
	}
	if _, ok := cfg.NamedRequire["other"]; !ok {
		t.Error("an unrelated named entry was removed")
	}
}

// T1524: a URL carrying the `repo//subdir` spelling of another ecosystem must be
// refused by `package add`/`package pin` rather than written into a manifest that
// ParseConfig then rejects.
func TestAddRejectsDoubleSlashURL(t *testing.T) {
	if os.Getenv("TEST_ADD_DOUBLE_SLASH") == "1" {
		dir := t.TempDir()
		os.Chdir(dir)
		os.WriteFile(filepath.Join(dir, "promise.toml"),
			[]byte("[module]\nname = \"app\"\nepoch = \"2026.0\"\n"), 0644)
		runAdd([]string{"https://github.com/acme/base//proto/wire"})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestAddRejectsDoubleSlashURL")
	cmd.Env = append(os.Environ(), "TEST_ADD_DOUBLE_SLASH=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a non-zero exit for a '//' URL, got:\n%s", out)
	}
	if !strings.Contains(string(out), "subdir") {
		t.Errorf("expected the error to point at the 'subdir' field, got:\n%s", out)
	}
}

// A subdir module with no matching pin must be reported against the module, not
// the bare repo: the hint has to name the [require.NAME] form, since a flat
// [require] line cannot carry a subdir at all (T1524).
func TestLoadRemoteSubdirNoPinHint(t *testing.T) {
	projectDir := t.TempDir()
	cfg := &module.Config{
		Name:    "app",
		Epoch:   "2026.0",
		Dir:     projectDir,
		Require: map[string]string{},
		Replace: map[string]string{},
	}
	loader := testModuleLoaderWithConfig(projectDir, cfg)

	_, err := loader.loadRemote("https://github.com/acme/base", "proto/wire", "wire")
	if err == nil {
		t.Fatal("expected a no-pin error")
	}
	msg := err.Error()
	for _, want := range []string{"no pin", "proto/wire", "[require.wire]", "subdir"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q is missing %q", msg, want)
		}
	}
	// The flat-[require] hint would be actively wrong here — a flat key is the URL
	// and has nowhere to put the subdir.
	if strings.Contains(msg, "package pin") {
		t.Errorf("error %q suggests `package pin`, which cannot express a subdir", msg)
	}
}

// A pin recorded for one subdir module must not satisfy its sibling in the same
// repo: identities differ, so each [require.NAME] entry needs its own pin.
func TestSubdirPinsAreNotSharedAcrossSiblings(t *testing.T) {
	projectDir := t.TempDir()
	cfg := &module.Config{
		Name:    "app",
		Epoch:   "2026.0",
		Dir:     projectDir,
		Require: map[string]string{},
		Replace: map[string]string{},
		NamedRequire: map[string]*module.RequireEntry{
			"wire": {URL: "https://github.com/acme/base", Commit: "abc123", Subdir: "proto/wire"},
		},
	}
	loader := testModuleLoaderWithConfig(projectDir, cfg)

	// The sibling has no entry of its own — resolution must stop at the pin
	// lookup rather than silently reusing 'wire''s commit.
	_, err := loader.loadRemote("https://github.com/acme/base", "proto/types", "types")
	if err == nil {
		t.Fatal("expected a no-pin error for the unpinned sibling")
	}
	if !strings.Contains(err.Error(), "no pin") {
		t.Errorf("expected a no-pin error, got: %v", err)
	}
	// And the repo root, addressed with no subdir, is a third identity.
	_, err = loader.loadRemote("https://github.com/acme/base", "", "base")
	if err == nil {
		t.Fatal("expected a no-pin error for the repo root")
	}
	if !strings.Contains(err.Error(), "no pin") {
		t.Errorf("expected a no-pin error, got: %v", err)
	}
}

// runAdd rejects every unusable --subdir/--name combination before it touches the
// manifest. Each case runs in a subprocess because the failures call os.Exit(1).
func TestAddSubdirFlagValidation(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantText string
	}{
		// A catalog name resolves to a URL the user does not control, and its
		// import name is fixed by the catalog.
		{"catalog_name_with_subdir", []string{"--subdir", "proto/wire", "json"}, "require a git URL"},
		{"catalog_name_with_name", []string{"--name", "w", "json"}, "require a git URL"},
		// An escaping subdir must never reach the manifest.
		{"escaping_subdir", []string{"--subdir", "../outside", "https://github.com/acme/base"}, "invalid --subdir"},
		{"absolute_subdir", []string{"--subdir", "/etc", "https://github.com/acme/base"}, "invalid --subdir"},
		// The derived import name is the last subdir component; if that is not an
		// identifier, `use NAME;` could never name it.
		{"underivable_name", []string{"--subdir", "proto/my-wire", "https://github.com/acme/base"}, "not a usable import name"},
		{"explicit_bad_name", []string{"--subdir", "proto/wire", "--name", "9wire", "https://github.com/acme/base"}, "not a usable import name"},
		// A [require.NAME] entry is shadowed by a catalog module of the same name.
		{"catalog_name_collision", []string{"--subdir", "proto/json", "https://github.com/acme/base"}, "conflicts with catalog module"},
		// A flag with no value.
		{"missing_flag_value", []string{"https://github.com/acme/base", "--subdir"}, "requires a value"},
	}

	if idx := os.Getenv("TEST_ADD_FLAG_CASE"); idx != "" {
		for _, tc := range cases {
			if tc.name != idx {
				continue
			}
			dir := t.TempDir()
			os.Chdir(dir)
			os.WriteFile(filepath.Join(dir, "promise.toml"),
				[]byte("[module]\nname = \"app\"\nepoch = \"2026.0\"\n"), 0644)
			runAdd(tc.args)
			return
		}
		os.Exit(9) // unknown case name — fail loudly
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=TestAddSubdirFlagValidation")
			cmd.Env = append(os.Environ(), "TEST_ADD_FLAG_CASE="+tc.name)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("expected a non-zero exit, got:\n%s", out)
			}
			if !strings.Contains(string(out), tc.wantText) {
				t.Errorf("expected %q in output, got:\n%s", tc.wantText, out)
			}
			// Nothing may have been written to the manifest.
			if strings.Contains(string(out), "Added ") {
				t.Errorf("a rejected add still reported success:\n%s", out)
			}
		})
	}
}

// `promise package pin` writes a flat [require] key, which cannot carry a subdir —
// so the `repo//subdir` spelling has to be refused there too, not just in `add`.
func TestPinRejectsDoubleSlashURL(t *testing.T) {
	if os.Getenv("TEST_PIN_DOUBLE_SLASH") == "1" {
		dir := t.TempDir()
		os.Chdir(dir)
		os.WriteFile(filepath.Join(dir, "promise.toml"),
			[]byte("[module]\nname = \"app\"\nepoch = \"2026.0\"\n"), 0644)
		runPin([]string{"https://github.com/acme/base//proto/wire"})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestPinRejectsDoubleSlashURL")
	cmd.Env = append(os.Environ(), "TEST_PIN_DOUBLE_SLASH=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a non-zero exit for a '//' URL, got:\n%s", out)
	}
	if !strings.Contains(string(out), "subdir") {
		t.Errorf("expected the error to point at the 'subdir' field, got:\n%s", out)
	}
}

// Two modules in one repo are pinned independently, so a project may sit on
// different commits for each. The bare repo is shared but checkouts are keyed on
// (url, commit), so both must resolve — and each must see the source from *its*
// commit, not from whichever one was fetched first (T1524).
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
	cli := newCLIEnv(t)

	work := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(work, 0755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "init", "--initial-branch=main")
	gitRun(t, work, "config", "user.email", "test@test.com")
	gitRun(t, work, "config", "user.name", "Test")

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
	gitRun(t, work, "add", ".")
	gitRun(t, work, "commit", "-m", "v1")
	commit1 := gitRun(t, work, "rev-parse", "HEAD")

	// Commit 2: only `types` moves forward.
	writeSub("proto/types", "types", "types-v2")
	gitRun(t, work, "add", ".")
	gitRun(t, work, "commit", "-m", "v2")
	commit2 := gitRun(t, work, "rev-parse", "HEAD")

	repo := filepath.Join(t.TempDir(), "base.git")
	gitRun(t, "", "clone", "--bare", "--quiet", work, repo)

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

	outStr, err := cli.promise(t, dir, "run", ".")
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
func TestUpdateNamedSubdirEntry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping verify integration test in short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("local repo paths contain ':' which is invalid in Windows cache paths")
	}
	t.Parallel()
	cli := newCLIEnv(t)
	epoch := compilerEpochForTest(t)

	bareDir := filepath.ToSlash(shortRepoDir(t))
	workDir := shortRepoDir(t)
	projDir := t.TempDir()

	run := func(dir, name string, args ...string) { cli.run(t, dir, name, args...) }
	headOf := func(dir string) string { return cli.git(t, dir, "rev-parse", "HEAD") }

	run(bareDir, "git", "init", "--bare", ".")
	run(workDir, "git", "clone", bareDir, ".")
	// The repo root deliberately has no promise.toml — only proto/wire does.
	sub := filepath.Join(workDir, "proto", "wire")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	writeMod(t, sub, "wire", true)
	run(workDir, "git", "add", ".")
	run(workDir, "git", "commit", "-m", "first")
	run(workDir, "git", "push", "origin", "HEAD")
	oldHash := headOf(workDir)

	// Second commit carries the epoch tag the update should move to.
	if err := os.WriteFile(filepath.Join(sub, "extra.pr"),
		[]byte("extra_value() int `public { return 9; }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run(workDir, "git", "add", ".")
	run(workDir, "git", "commit", "-m", "second")
	run(workDir, "git", "tag", "epoch-"+epoch)
	run(workDir, "git", "push", "origin", "HEAD", "--tags")
	headHash := headOf(workDir)

	toml := "[module]\nname = \"proj\"\nepoch = \"" + epoch + "\"\n\n" +
		"[require.wire]\nurl = \"" + bareDir + "\"\ncommit = \"" + oldHash + "\"\nsubdir = \"proto/wire\"\n"
	if err := os.WriteFile(filepath.Join(projDir, "promise.toml"), []byte(toml), 0644); err != nil {
		t.Fatal(err)
	}

	out := cli.promiseOK(t, projDir, "package", "update")
	if !strings.Contains(out, "Updated 1 of 1") {
		t.Fatalf("expected 'Updated 1 of 1', got: %s", out)
	}

	cfg, err := module.ParseConfig(filepath.Join(projDir, "promise.toml"))
	if err != nil {
		t.Fatalf("ParseConfig after update: %v", err)
	}
	e := cfg.NamedRequire["wire"]
	if e == nil {
		t.Fatalf("[require.wire] vanished: %+v", cfg.NamedRequire)
	}
	if e.Commit != headHash {
		t.Errorf("commit = %q, want the epoch-tagged head %q", e.Commit, headHash)
	}
	if e.Subdir != "proto/wire" {
		t.Errorf("subdir = %q, want it preserved across the update", e.Subdir)
	}
}

// `promise package add --name w <url>` (no --subdir) writes a named entry for a
// repo-ROOT module: the same [require.NAME] shape, minus the subdir line. This is
// how a dependency gets a stable import name that differs from its URL.
func TestAddNamedWithoutSubdir(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping verify integration test in short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("local repo paths contain ':' which is invalid in Windows cache paths")
	}
	t.Parallel()
	cli := newCLIEnv(t)
	epoch := compilerEpochForTest(t)

	bareDir := filepath.ToSlash(shortRepoDir(t))
	workDir := shortRepoDir(t)
	projDir := t.TempDir()

	run := func(dir, name string, args ...string) { cli.run(t, dir, name, args...) }

	run(bareDir, "git", "init", "--bare", ".")
	run(workDir, "git", "clone", bareDir, ".")
	writeMod(t, workDir, "dep", true)
	run(workDir, "git", "add", ".")
	run(workDir, "git", "commit", "-m", "first")
	run(workDir, "git", "tag", "v1.0")
	run(workDir, "git", "push", "origin", "HEAD", "--tags")

	if err := os.WriteFile(filepath.Join(projDir, "promise.toml"),
		[]byte("[module]\nname = \"proj\"\nepoch = \""+epoch+"\"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	out := cli.promiseOK(t, projDir, "package", "add", "--name", "helper", bareDir, "v1.0")
	if !strings.Contains(out, "use helper;") {
		t.Errorf("expected the import hint to use the --name value, got: %s", out)
	}

	text, err := os.ReadFile(filepath.Join(projDir, "promise.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(text), "subdir") {
		t.Errorf("a root-addressed named entry must write no subdir line:\n%s", text)
	}
	cfg, err := module.ParseConfig(filepath.Join(projDir, "promise.toml"))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	e := cfg.NamedRequire["helper"]
	if e == nil || e.Commit == "" || e.Subdir != "" {
		t.Fatalf("entry = %+v, want a pinned root-addressed named entry", e)
	}
	if len(cfg.Require) != 0 {
		t.Errorf("--name must not write a flat [require] entry, got %v", cfg.Require)
	}
}

// A catalog entry may also address a subdirectory (catalog.toml grew a `subdir`
// key alongside url/commit), so loadCatalog must resolve into it rather than the
// repo root. Exercised with a synthetic catalog over a local bare repo — the
// embedded catalog ships no subdir entry today, so nothing else covers this path.
func TestLoadCatalogSubdirEntry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping git integration test in short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("local repo paths contain ':' which is invalid in Windows cache paths")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repo, commit := makeSubdirRepo(t, map[string]string{"wire": "proto/wire"})

	saved := embeddedCatalog
	// std must stay in the catalog — it is auto-imported into every file.
	embeddedCatalog = []byte("[catalog]\nepoch = \"2026.0\"\n\n" +
		"[modules.std]\ndescription = \"standard library\"\n\n" +
		"[modules.wire]\nurl = \"" + repo + "\"\n" +
		"commit = \"" + commit + "\"\nsubdir = \"proto/wire\"\ndescription = \"wire types\"\n")
	defer func() { embeddedCatalog = saved }()

	projectDir := t.TempDir()
	loader := testModuleLoaderWithConfig(projectDir, &module.Config{
		Name: "app", Epoch: "2026.0", Dir: projectDir,
		Require: map[string]string{}, Replace: map[string]string{},
	})
	if loader.catalog == nil {
		t.Fatal("synthetic catalog did not parse")
	}
	if e := loader.catalog.Lookup("wire"); e == nil || e.Subdir != "proto/wire" {
		t.Fatalf("catalog entry = %+v, want subdir proto/wire", e)
	}

	mi, err := loader.loadCatalog("wire")
	if err != nil {
		t.Fatalf("loadCatalog: %v", err)
	}
	if mi == nil {
		t.Fatal("loadCatalog returned no module info")
	}
	if filepath.Base(mi.AbsDir) != "wire" {
		t.Errorf("resolved to %q, want the proto/wire subdirectory", mi.AbsDir)
	}
	if _, err := os.Stat(filepath.Join(mi.AbsDir, "promise.toml")); err != nil {
		t.Errorf("resolved directory has no manifest: %v", err)
	}
	// A catalog module keeps its catalog identity — the subdir affects where the
	// sources come from, not how the module is named in IR.
	if mi.GlobalIdentity != module.GlobalIdentityForCatalog("wire") {
		t.Errorf("identity = %q, want the catalog identity", mi.GlobalIdentity)
	}
}

// The CLI argument guard does not cover a URL the *catalog* supplies. ParseConfig
// rejects a '//' URL, so writing one would produce a manifest the next command
// cannot read — `add` has to refuse it instead (T1524).
func TestAddRejectsCatalogSuppliedDoubleSlashURL(t *testing.T) {
	if os.Getenv("TEST_ADD_CATALOG_DOUBLE_SLASH") == "1" {
		embeddedCatalog = []byte("[catalog]\nepoch = \"2026.0\"\n\n" +
			"[modules.wire]\nurl = \"https://github.com/acme/base//proto/wire\"\n" +
			"commit = \"abc123\"\ndescription = \"bad url\"\n")
		dir := t.TempDir()
		os.Chdir(dir)
		os.WriteFile(filepath.Join(dir, "promise.toml"),
			[]byte("[module]\nname = \"app\"\nepoch = \"2026.0\"\n"), 0644)
		runAdd([]string{"wire"})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestAddRejectsCatalogSuppliedDoubleSlashURL")
	cmd.Env = append(os.Environ(), "TEST_ADD_CATALOG_DOUBLE_SLASH=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a non-zero exit for a catalog '//' URL, got:\n%s", out)
	}
	if !strings.Contains(string(out), "catalog url") {
		t.Errorf("expected the error to name the catalog URL, got:\n%s", out)
	}
}

// One URL can appear in both tables at once: a flat [require] key from before the
// module moved into a subdirectory, plus the [require.NAME] entries that replaced
// it. `remove <url>` must clear all of them in a single pass, leaving a manifest
// that still parses.
func TestPackageRemoveMixedFlatAndNamed(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	manifest := `[module]
name = "app"
epoch = "2026.0"

[require]
"https://github.com/acme/base" = "abc123"
"https://github.com/acme/keep" = "999999"

[require.wire]
url = "https://github.com/acme/base"
commit = "abc123"
subdir = "proto/wire"
`
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		captureStderr(func() { runPackageRemove([]string{"github.com/acme/base"}) })
	})
	if !strings.Contains(out, "[require.wire]") {
		t.Errorf("expected the named entry to be reported removed, got: %s", out)
	}

	cfg, err := module.ParseConfig(filepath.Join(dir, "promise.toml"))
	if err != nil {
		t.Fatalf("ParseConfig after removal: %v", err)
	}
	for url := range cfg.Require {
		if module.NormalizeURL(url) == module.NormalizeURL("https://github.com/acme/base") {
			t.Errorf("the flat [require] key survived removal: %q", url)
		}
	}
	if _, ok := cfg.NamedRequire["wire"]; ok {
		t.Error("[require.wire] survived removal by its repo URL")
	}
	if cfg.Require["https://github.com/acme/keep"] != "999999" {
		t.Errorf("an unrelated flat entry was disturbed: %v", cfg.Require)
	}
}
