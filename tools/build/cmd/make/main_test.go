package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/promise-language/promise/tools/build/common"
)

// under is the seams the test running now has set up (stubSeams), which
// runMake hands to run.
var under seams

// stubSeams replaces every seam for one test: hooks wiring does nothing, and a
// build is recorded and writes a placeholder binary. It
// returns the names built and the ldflags each was given, in order.
func stubSeams(t *testing.T) (built *[]string, ldflags *[]string) {
	t.Helper()
	t.Cleanup(func() {
		under = seams{}
		common.SetObserved(true)
	})
	under.setupHooks = func(string) error { return nil }
	var names, flags []string
	under.buildTool = func(root, name, out, lf string) error {
		names = append(names, name)
		flags = append(flags, lf)
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		return os.WriteFile(out, []byte("binary "+name), 0o755)
	}
	return &names, &flags
}

// forbidBuild makes any build attempt fail the test.
func forbidBuild(t *testing.T) {
	t.Helper()
	under.buildTool = func(root, name, out, lf string) error {
		t.Errorf("unexpected build of %s", name)
		return nil
	}
}

// fakeRoot lays out the minimum run needs: a .gitignore holding the given
// lines, and tools/build/cmd with the given tools plus make. It is not a git
// checkout: building the tools needs nothing but Go.
func fakeRoot(t *testing.T, ignore []string, tools ...string) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".gitignore"), strings.Join(ignore, "\n")+"\n")
	for _, name := range append([]string{"make"}, tools...) {
		writeFile(t, filepath.Join(root, "tools", "build", "cmd", name, "main.go"), "package main\n\nfunc main() {}\n")
	}
	return root
}

var allIgnored = []string{"/bin/", "/.workspace/", "/.home/"}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// markUpToDate makes bin/ look freshly built from root's sources.
func markUpToDate(t *testing.T, root string, tools ...string) {
	t.Helper()
	hash, err := common.ToolsSourceHash(root)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "bin", ".tools.hash"), hash+"\n")
	for _, name := range tools {
		writeFile(t, filepath.Join(root, "bin", name+common.ExeSuffix()), "binary")
	}
}

func runMake(t *testing.T, root string, argv ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errb bytes.Buffer
	err = run(root, argv, &out, &errb, under)
	return out.String(), errb.String(), err
}

// decodeResult decodes stdout as exactly one JSON result line.
func decodeResult(t *testing.T, stdout string) map[string]any {
	t.Helper()
	if strings.Count(stdout, "\n") != 1 || !strings.HasSuffix(stdout, "\n") {
		t.Fatalf("stdout is not one line: %q", stdout)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"built", "removed", "up_to_date"}; !slices.Equal(keys, want) {
		t.Fatalf("result keys = %v, want %v", keys, want)
	}
	return got
}

func names(t *testing.T, v any) []string {
	t.Helper()
	items, ok := v.([]any)
	if !ok {
		t.Fatalf("%v is not an array", v)
	}
	out := []string{}
	for _, it := range items {
		out = append(out, it.(string))
	}
	return out
}

func TestHelpJSON(t *testing.T) {
	stubSeams(t)
	forbidBuild(t)
	stdout, stderr, err := runMake(t, t.TempDir(), "-help", "-json")
	if err != nil || stderr != "" {
		t.Fatalf("err=%v stderr=%q", err, stderr)
	}
	var doc struct {
		Project string `json:"project"`
		Flags   []struct {
			Name      string   `json:"name"`
			Spellings []string `json:"spellings"`
		} `json:"flags"`
		Commands []any `json:"commands"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("-help -json is not JSON: %v\n%s", err, stdout)
	}
	if doc.Project != "promise" {
		t.Errorf("project = %q, want promise", doc.Project)
	}
	if len(doc.Commands) != 0 {
		t.Errorf("commands = %v, want none", doc.Commands)
	}
	i := slices.IndexFunc(doc.Flags, func(f struct {
		Name      string   `json:"name"`
		Spellings []string `json:"spellings"`
	}) bool {
		return f.Name == "rebuild"
	})
	if i < 0 {
		t.Fatalf("no rebuild flag in %s", stdout)
	}
	if got := doc.Flags[i].Spellings; !slices.Equal(got, []string{"rebuild", "no-rebuild"}) {
		t.Errorf("rebuild spellings = %v", got)
	}
}

func TestVersionJSON(t *testing.T) {
	stubSeams(t)
	forbidBuild(t)
	root := fakeRoot(t, allIgnored, "alpha")
	want, err := common.ToolsSourceHash(root)
	if err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runMake(t, root, "-version", "-json")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("-version -json is not JSON: %v\n%s", err, stdout)
	}
	// The hash it computes is the one every tool it builds is stamped with.
	if got["project"] != "promise" || got["text"] != want || len(got) != 2 {
		t.Errorf("-version -json = %v, want project promise, text %q", got, want)
	}
}

func TestUsageErrors(t *testing.T) {
	for _, argv := range [][]string{{"-bogus"}, {"-force"}, {"-json", "-human"}, {"build"}, {"-rebuild", "-help"}} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			stubSeams(t)
			forbidBuild(t)
			root := fakeRoot(t, allIgnored, "alpha")
			stdout, _, err := runMake(t, root, argv...)
			if got := common.ExitCode(err); got != 2 {
				t.Errorf("exit = %d (err %v), want 2", got, err)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
		})
	}
}

func TestJSONResultUpToDate(t *testing.T) {
	stubSeams(t)
	forbidBuild(t)
	root := fakeRoot(t, allIgnored, "alpha", "beta")
	markUpToDate(t, root, "alpha", "beta")
	stdout, _, err := runMake(t, root, "-json")
	if err != nil {
		t.Fatal(err)
	}
	got := decodeResult(t, stdout)
	if got["up_to_date"] != true {
		t.Errorf("up_to_date = %v, want true", got["up_to_date"])
	}
	if b := names(t, got["built"]); len(b) != 0 {
		t.Errorf("built = %v, want []", b)
	}
	if r := names(t, got["removed"]); len(r) != 0 {
		t.Errorf("removed = %v, want []", r)
	}
}

func TestJSONResultBuilt(t *testing.T) {
	built, ldflags := stubSeams(t)
	root := fakeRoot(t, allIgnored, "alpha", "beta")
	stdout, _, err := runMake(t, root, "-json")
	if err != nil {
		t.Fatal(err)
	}
	got := decodeResult(t, stdout)
	if got["up_to_date"] != false {
		t.Errorf("up_to_date = %v, want false", got["up_to_date"])
	}
	if b := names(t, got["built"]); !slices.Equal(b, []string{"alpha", "beta"}) {
		t.Errorf("built = %v, want [alpha beta]", b)
	}
	if r := names(t, got["removed"]); len(r) != 0 {
		t.Errorf("removed = %v, want []", r)
	}
	if !slices.Equal(*built, []string{"alpha", "beta"}) {
		t.Errorf("build calls = %v", *built)
	}
	hash, _ := common.ToolsSourceHash(root)
	stamp := "-X main.sourceHash=" + hash
	for _, lf := range *ldflags {
		if !strings.Contains(lf, stamp) {
			t.Errorf("ldflags %q lack %q", lf, stamp)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(root, "bin", ".tools.hash")); strings.TrimSpace(string(data)) != hash {
		t.Errorf(".tools.hash = %q, want %q", data, hash)
	}

	// The same sources again: nothing to build.
	*built = nil
	stdout, _, err = runMake(t, root, "-json")
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeResult(t, stdout); got["up_to_date"] != true || len(*built) != 0 {
		t.Errorf("second run: %v, built %v; want up to date with nothing built", got, *built)
	}
}

func TestRebuildForcesBuild(t *testing.T) {
	built, _ := stubSeams(t)
	root := fakeRoot(t, allIgnored, "alpha", "beta")
	markUpToDate(t, root, "alpha", "beta")
	stdout, _, err := runMake(t, root, "-rebuild", "-json")
	if err != nil {
		t.Fatal(err)
	}
	got := decodeResult(t, stdout)
	if got["up_to_date"] != false || !slices.Equal(names(t, got["built"]), []string{"alpha", "beta"}) {
		t.Errorf("-rebuild result = %v, want everything built", got)
	}
	if !slices.Equal(*built, []string{"alpha", "beta"}) {
		t.Errorf("build calls = %v", *built)
	}

	// -no-rebuild is the default spelled out.
	*built = nil
	if _, _, err := runMake(t, root, "-no-rebuild", "-json"); err != nil || len(*built) != 0 {
		t.Errorf("-no-rebuild: err %v, built %v; want nothing built", err, *built)
	}
}

func TestHumanResult(t *testing.T) {
	stubSeams(t)
	root := fakeRoot(t, allIgnored, "alpha")
	stdout, _, err := runMake(t, root, "-human")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "1 tools built\n") || !strings.Contains(stdout, "done (") {
		t.Errorf("human stdout = %q", stdout)
	}
	if strings.Contains(stdout, "Building") || strings.Contains(stdout, "alpha") {
		t.Errorf("progress reached stdout: %q", stdout)
	}
	stdout, _, err = runMake(t, root, "-human")
	if err != nil || !strings.HasPrefix(stdout, "Tools up to date (1 tools, hash: ") {
		t.Errorf("second run: err %v, stdout %q", err, stdout)
	}
}

func TestNarrationGoesToStderr(t *testing.T) {
	stubSeams(t)
	root := fakeRoot(t, allIgnored, "alpha")
	stdout, stderr, err := runMake(t, root, "-json", "-narrate")
	if err != nil {
		t.Fatal(err)
	}
	decodeResult(t, stdout)
	for _, want := range []string{"Building 1 tools", "alpha", "1 tools built", "done ("} {
		if !strings.Contains(stderr, want) {
			t.Errorf("narration lacks %q: %q", want, stderr)
		}
	}
}

func TestBuildFailure(t *testing.T) {
	stubSeams(t)
	succeed := under.buildTool
	under.buildTool = func(root, name, out, lf string) error {
		if name == "beta" {
			return os.ErrPermission
		}
		return succeed(root, name, out, lf)
	}
	root := fakeRoot(t, allIgnored, "alpha", "beta")
	stdout, stderr, err := runMake(t, root, "-json")
	if common.ExitCode(err) != 1 || !strings.Contains(err.Error(), "1/2 tools failed") {
		t.Errorf("err = %v, want 1/2 tools failed (exit 1)", err)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "FAIL beta") {
		t.Errorf("stderr = %q, want the failure named", stderr)
	}
	if _, err := os.Stat(filepath.Join(root, "bin", ".tools.hash")); err == nil {
		t.Errorf(".tools.hash written after a failed build")
	}
}

func TestRefusesCheckoutNotIgnoring(t *testing.T) {
	stubSeams(t)
	forbidBuild(t)
	root := fakeRoot(t, []string{"/bin/", "/.workspace/"}, "alpha")
	stdout, _, err := runMake(t, root, "-json")
	if common.ExitCode(err) != 1 {
		t.Fatalf("err = %v, want a refusal (exit 1)", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, ".home/ is not ignored") || !strings.Contains(msg, "add the line /.home/ to .gitignore") {
		t.Errorf("refusal %q does not name .home/ and its line", msg)
	}
	if strings.Contains(msg, "bin/ is not") || strings.Contains(msg, ".workspace/ is not") {
		t.Errorf("refusal %q names an ignored path", msg)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
}

// TestReadsTheCheckoutsOwnGitignore: any spelling that ignores the top-level
// directory will do, and nothing but the committed .gitignore counts — a line
// in this clone's .git/info/exclude ignores nothing for the next clone.
func TestReadsTheCheckoutsOwnGitignore(t *testing.T) {
	stubSeams(t)
	root := fakeRoot(t, []string{"# tools", "bin/", "  /.workspace  ", ".home"}, "alpha")
	if _, _, err := runMake(t, root, "-json"); err != nil {
		t.Errorf("err = %v, want every spelling accepted", err)
	}
	root = fakeRoot(t, []string{"/bin/", "/.workspace/"}, "alpha")
	writeFile(t, filepath.Join(root, ".git", "info", "exclude"), "/.home/\n")
	if _, _, err := runMake(t, root, "-json"); err == nil || !strings.Contains(err.Error(), ".home/ is not ignored") {
		t.Errorf("err = %v, want .home/ refused although .git/info/exclude lists it", err)
	}
}

func TestRefusesWorkspaceToolCollision(t *testing.T) {
	stubSeams(t)
	forbidBuild(t)
	root := fakeRoot(t, allIgnored, "alpha", "beta")
	writeFile(t, filepath.Join(root, ".workspace", "project.json"), `{"tools":["beta","issue"]}`)
	stdout, _, err := runMake(t, root, "-json")
	if common.ExitCode(err) != 1 {
		t.Fatalf("err = %v, want a refusal (exit 1)", err)
	}
	want := "./make will not build beta: .workspace/project.json lists it as a workspace tool a setup installed"
	if err.Error() != want {
		t.Errorf("refusal = %q, want %q", err, want)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
}

func TestWorkspaceToolsWithoutCollision(t *testing.T) {
	built, _ := stubSeams(t)
	root := fakeRoot(t, allIgnored, "alpha")
	writeFile(t, filepath.Join(root, ".workspace", "project.json"), `{"repo":"x","tools":["issue"]}`)
	if _, _, err := runMake(t, root, "-json"); err != nil || !slices.Equal(*built, []string{"alpha"}) {
		t.Errorf("err %v, built %v; want alpha built", err, *built)
	}
}

func TestRefusesUnparseableWorkspaceProject(t *testing.T) {
	stubSeams(t)
	forbidBuild(t)
	root := fakeRoot(t, allIgnored, "alpha")
	writeFile(t, filepath.Join(root, ".workspace", "project.json"), `{"tools":`)
	_, _, err := runMake(t, root, "-json")
	if common.ExitCode(err) != 1 || !strings.Contains(err.Error(), ".workspace/project.json") {
		t.Errorf("err = %v, want a refusal naming .workspace/project.json", err)
	}
}

// TestWiresHooksOnEveryRun: ./make is the bootstrap, so the hooks are wired
// whether it builds or finds bin/ current — a clone whose tools were built
// before the hooks existed gets them on its next ./make.
func TestWiresHooksOnEveryRun(t *testing.T) {
	stubSeams(t)
	wired := 0
	under.setupHooks = func(string) error { wired++; return nil }
	root := fakeRoot(t, allIgnored, "alpha")
	if _, _, err := runMake(t, root, "-json"); err != nil || wired != 1 {
		t.Fatalf("a building run: err = %v, wired %d times, want once", err, wired)
	}
	forbidBuild(t)
	if _, _, err := runMake(t, root, "-json"); err != nil || wired != 2 {
		t.Errorf("an up-to-date run: err = %v, wired %d times in all, want twice", err, wired)
	}
}
