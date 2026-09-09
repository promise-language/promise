package buildrun

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
)

// srcDirProbe prints os.src_dir and os.working_dir on their own labelled lines,
// so a test can assert on each independently. The absent branch prints a
// sentinel rather than nothing, so "no source dir" and "no output" cannot be
// confused.
const srcDirProbe = `use os;
main!() {
  if d := os.src_dir {
    print_line("src_dir=" + d);
  } else {
    print_line("src_dir=<none>");
  }
  print_line("working_dir=" + os.working_dir?!);
}
`

// runProbe runs a command in dir under the package's shared PROMISE_HOME
// (clitest.IsolateHome, in TestMain) and returns its stdout. Stderr is captured
// separately rather than combined: the probe's answers are "key=value" lines on
// stdout, and folding the compiler's own diagnostics in would let one
// masquerade as an answer.
func runProbe(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %v in %s: %v\nstdout: %s\nstderr: %s",
			name, args, dir, err, stdout.String(), stderr.String())
	}
	return stdout.String()
}

// probeField returns the value of the named "key=value" line in out.
func probeField(t *testing.T, out, key string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if v, ok := strings.CutPrefix(line, key+"="); ok {
			return v
		}
	}
	t.Fatalf("no %s= line in output:\n%s", key, out)
	return ""
}

// resolvedDir is what os.working_dir (getcwd) will report for dir. t.TempDir()
// hands back a path under a symlink on macOS (/var → /private/var), and getcwd
// resolves it while filepath.Abs deliberately does not — os.src_dir is exactly
// the directory the caller named, so only the cwd side needs resolving.
func resolvedDir(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve %s: %v", dir, err)
	}
	return resolved
}

// assertSameDir compares two paths as directories rather than as strings. A
// relative argument is resolved against the invoking cwd, whose spelling the
// caller chooses: a shell that reached the directory through a symlink (and
// os/exec, which sets the child's PWD to Cmd.Dir) reports the symlinked path,
// while an absolute argument is baked exactly as given. Both name one directory,
// so the assertion is on identity, not on spelling.
func assertSameDir(t *testing.T, got, want, what string) {
	t.Helper()
	if got == want {
		return
	}
	gotReal, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Errorf("%s = %q, want %q (and it does not resolve: %v)", what, got, want, err)
		return
	}
	if gotReal != resolvedDir(t, want) {
		t.Errorf("%s = %q (resolving to %q), want the directory %q", what, got, gotReal, want)
	}
}

// writeProbe writes the probe program into dir under name and returns its path.
func writeProbe(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(srcDirProbe), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRunSrcDirIsSourceNotCwd is the regression test for T1521's core complaint,
// the one that defeated `go run`: a tool must be able to find where it lives
// independently of where it was invoked. The program lives in dir A and is
// invoked with the cwd set to an unrelated dir B, so a value that tracked the
// cwd — or the build-cache path the binary actually runs from — fails here.
func TestRunSrcDirIsSourceNotCwd(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping run integration test in short mode")
	}
	bin := clitest.Bin(t)

	srcDir := t.TempDir()
	cwdDir := t.TempDir()
	if srcDir == cwdDir {
		t.Fatal("test setup is broken: source and invocation directories must differ")
	}
	probe := writeProbe(t, srcDir, "probe.pr")

	out := runProbe(t, cwdDir, bin, "run", probe)

	if got, want := probeField(t, out, "src_dir"), srcDir; got != want {
		t.Errorf("os.src_dir = %q, want the program's own directory %q", got, want)
	}
	if got, want := probeField(t, out, "working_dir"), resolvedDir(t, cwdDir); got != want {
		t.Errorf("os.working_dir = %q, want the invocation directory %q", got, want)
	}
}

// TestRunProjectSrcDirIsProjectDir covers the project frontend: for a directory
// with a promise.toml, "where do I live" is the project directory, not the
// directory of whichever .pr file holds main().
func TestRunProjectSrcDirIsProjectDir(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping run integration test in short mode")
	}
	bin := clitest.Bin(t)

	projDir := t.TempDir()
	cwdDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projDir, "promise.toml"),
		[]byte("[module]\nname = \"probe\"\nepoch = \"2026.0\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	writeProbe(t, projDir, "main.pr")

	out := runProbe(t, cwdDir, bin, "run", projDir)

	if got, want := probeField(t, out, "src_dir"), projDir; got != want {
		t.Errorf("os.src_dir = %q, want the project directory %q", got, want)
	}
}

// TestExecHasNoSrcDir pins the edge the item calls out: an inline program does
// not live anywhere, so os.src_dir must be absent rather than a fabricated path
// (the cwd, or the temp binary's directory).
func TestExecHasNoSrcDir(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping exec integration test in short mode")
	}
	bin := clitest.Bin(t)

	cwdDir := t.TempDir()
	// autoInjectCatalogUses pulls in `use os;` from the bare os.src_dir reference.
	out := runProbe(t, cwdDir, bin, "exec",
		`if d := os.src_dir { print_line("src_dir=" + d); } else { print_line("src_dir=<none>"); }`)

	if got := probeField(t, out, "src_dir"); got != "<none>" {
		t.Errorf("os.src_dir under promise exec = %q, want <none> (an inline program has no source directory)", got)
	}
}

// TestRunSrcDirNotSharedBetweenIdenticalSources guards the build-cache keys. The
// run/test/project keys hash source *content*, so two byte-identical programs in
// different directories would otherwise share a cache entry and the second
// invocation would exec a binary carrying the first one's baked path. Both runs
// share this package's PROMISE_HOME, so the cache really is a common one.
func TestRunSrcDirNotSharedBetweenIdenticalSources(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping run integration test in short mode")
	}
	bin := clitest.Bin(t)

	dirA := t.TempDir()
	dirB := t.TempDir()
	probeA := writeProbe(t, dirA, "probe.pr")
	probeB := writeProbe(t, dirB, "probe.pr")

	outA := runProbe(t, dirA, bin, "run", probeA)
	outB := runProbe(t, dirB, bin, "run", probeB)

	if got, want := probeField(t, outA, "src_dir"), dirA; got != want {
		t.Errorf("first run: os.src_dir = %q, want %q", got, want)
	}
	if got, want := probeField(t, outB, "src_dir"), dirB; got != want {
		t.Errorf("second run of byte-identical source: os.src_dir = %q, want %q "+
			"(the build cache handed back the first program's binary)", got, want)
	}
}

// TestBuiltBinaryKeepsSrcDir confirms the value is a property of the binary, not
// of the command that produced it or the directory it is later run from:
// `promise build` bakes the same directory `promise run` does, and it survives
// being executed from somewhere else entirely.
func TestBuiltBinaryKeepsSrcDir(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping build integration test in short mode")
	}
	bin := clitest.Bin(t)

	srcDir := t.TempDir()
	outDir := t.TempDir()
	runDir := t.TempDir()
	probe := writeProbe(t, srcDir, "probe.pr")
	binary := filepath.Join(outDir, "probe")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}

	runProbe(t, outDir, bin, "build", probe, "-o", binary)

	out := runProbe(t, runDir, binary)

	if got, want := probeField(t, out, "src_dir"), srcDir; got != want {
		t.Errorf("built binary: os.src_dir = %q, want the source directory %q", got, want)
	}
	if got, want := probeField(t, out, "working_dir"), resolvedDir(t, runDir); got != want {
		t.Errorf("built binary: os.working_dir = %q, want the invocation directory %q", got, want)
	}
}

// TestRunSrcDirFromRelativeArgument covers the spellings a tool is actually
// invoked with. `promise run probe.pr` and `promise run ../probe.pr` name the
// same program by a path relative to two different cwds, and both must bake the
// same absolute, cleaned directory: a caller that gets "." or a path still
// containing ".." cannot join anything onto it and expect to find the repo root.
// The second invocation also proves the two spellings share one cache entry
// rather than each compiling their own copy.
func TestRunSrcDirFromRelativeArgument(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping run integration test in short mode")
	}
	bin := clitest.Bin(t)

	srcDir := t.TempDir()
	subDir := filepath.Join(srcDir, "sub")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeProbe(t, srcDir, "probe.pr")

	// Named relative to its own directory.
	out := runProbe(t, srcDir, bin, "run", "probe.pr")
	fromOwnDir := probeField(t, out, "src_dir")
	assertSameDir(t, fromOwnDir, srcDir, "os.src_dir for `run probe.pr`")

	// Named through "..", from a directory it does not live in.
	out = runProbe(t, subDir, bin, "run", filepath.Join("..", "probe.pr"))
	got := probeField(t, out, "src_dir")
	assertSameDir(t, got, srcDir, "os.src_dir for `run ../probe.pr`")
	if got != fromOwnDir {
		t.Errorf("two relative spellings of one program disagree: %q vs %q", fromOwnDir, got)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("os.src_dir = %q, want an absolute path", got)
	}
	if strings.Contains(got, "..") {
		t.Errorf("os.src_dir = %q, want a cleaned path with no %q segment", got, "..")
	}
	if strings.HasSuffix(got, string(os.PathSeparator)) {
		t.Errorf("os.src_dir = %q, want no trailing separator", got)
	}
	if cwd := probeField(t, out, "working_dir"); cwd == got {
		t.Errorf("os.working_dir = os.src_dir = %q, but the program was invoked from %q", cwd, subDir)
	}
}

// TestRunProjectSrcDirTrailingSeparator pins the documented shape of the value
// for the spelling a shell tab-completion produces: `promise run proj/` must bake
// "…/proj", not "…/proj/". A trailing separator survives into every path a tool
// builds by concatenation, and doubles up under the ones built with a join.
func TestRunProjectSrcDirTrailingSeparator(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping run integration test in short mode")
	}
	bin := clitest.Bin(t)

	projDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projDir, "promise.toml"),
		[]byte("[module]\nname = \"probe\"\nepoch = \"2026.0\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	writeProbe(t, projDir, "main.pr")

	out := runProbe(t, t.TempDir(), bin, "run", projDir+string(os.PathSeparator))

	if got, want := probeField(t, out, "src_dir"), projDir; got != want {
		t.Errorf("os.src_dir for a trailing-separator project argument = %q, want %q", got, want)
	}
}

// TestRunSrcDirVisibleFromImportedModule pins whose directory the accessor
// reports when the call is not in main: os.src_dir answers "where does the
// *program* live", so a helper inside an imported local module sees the program's
// project directory, not its own. The one baked constant comes from the program's
// sema.Info (the module loader deliberately leaves a module's unset), and this is
// the case that exercises codegen's rootInfo fallback — the os bridge body is
// synthesised while a module's Info is current.
func TestRunSrcDirVisibleFromImportedModule(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping run integration test in short mode")
	}
	bin := clitest.Bin(t)

	projDir := t.TempDir()
	depDir := filepath.Join(projDir, "dep")
	if err := os.MkdirAll(depDir, 0755); err != nil {
		t.Fatal(err)
	}
	write := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(projDir, "promise.toml"), "[module]\nname = \"probe\"\nepoch = \"2026.0\"\n")
	write(filepath.Join(depDir, "promise.toml"), "[module]\nname = \"dep\"\nepoch = \"2026.0\"\n")
	write(filepath.Join(depDir, "dep.pr"), `use os;

dep_src_dir() string? `+"`public"+` `+"`doc"+`("Returns the source directory the dep module observes.") {
  return os.src_dir;
}
`)
	write(filepath.Join(projDir, "main.pr"), `use os;
use dep "./dep";
main!() {
  if d := os.src_dir {
    print_line("src_dir=" + d);
  } else {
    print_line("src_dir=<none>");
  }
  if d := dep.dep_src_dir() {
    print_line("dep_src_dir=" + d);
  } else {
    print_line("dep_src_dir=<none>");
  }
}
`)

	out := runProbe(t, t.TempDir(), bin, "run", projDir)

	if got, want := probeField(t, out, "src_dir"), projDir; got != want {
		t.Errorf("os.src_dir in main = %q, want the project directory %q", got, want)
	}
	if got, want := probeField(t, out, "dep_src_dir"), projDir; got != want {
		t.Errorf("os.src_dir inside the imported module = %q, want the program's directory %q "+
			"(not the module's own %q)", got, want, depDir)
	}
}
