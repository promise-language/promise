// Command make builds all Promise build tools into bin/.
//
// Usage: ./make [-rebuild] [-json | -human] [-narrate]   (or -help, -version)
//
// This is the bootstrap entry point. It compiles every tool under
// tools/build/cmd/ (except itself) and places the binaries in bin/.
// Each binary gets the current tools source hash injected via ldflags
// so it can detect when it becomes stale; -version reports that hash.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/promise-language/promise/tools/build/common"
)

// spec is everything ./make accepts on its command line.
var spec = common.ToolSpec{
	Tool:        "make",
	Description: "build this project's tools into bin/",
	Flags: []common.FlagSpec{
		{Name: "rebuild", Description: "build every tool even when bin/ is up to date"},
	},
}

// The seams tests replace: each reaches outside the checkout's files (git's
// config, the Go toolchain).
var (
	setupHooks = wireHooks
	buildTool  = goBuild
)

func main() {
	root, err := bootstrapRoot()
	if err != nil {
		os.Exit(common.Fail("make", err))
	}
	os.Exit(common.Fail("make", run(root, os.Args[1:], os.Stdout, os.Stderr)))
}

// result is the JSON answer on stdout. Removed is always empty — ./make
// deletes nothing — and is there so the shape does not change if it ever does.
type result struct {
	UpToDate bool     `json:"up_to_date"`
	Built    []string `json:"built"`
	Removed  []string `json:"removed"`
}

// run is ./make for one command line. It writes the result, and only the
// result, on stdout; progress is narration, written to stderr only when someone
// is watching. The error it returns is what main hands to common.Fail: a
// *common.UsageError (exit 2) or a refusal or failure (exit 1).
func run(root string, argv []string, stdout, stderr io.Writer) error {
	start := time.Now()

	inv, err := common.ParseInvocation(spec, argv)
	if err != nil {
		return err
	}
	if inv.Help {
		return common.WriteHelp(stdout, spec, inv.Mode)
	}
	if inv.Version {
		// The hash it computes, which is the one every tool it builds is
		// stamped with and compares: ./make itself is never stale.
		text, err := common.ToolsSourceHash(root)
		if err != nil {
			text = "unknown"
		}
		return common.WriteVersion(stdout, inv.Mode, text)
	}

	observed := common.ObservedFor(inv)
	common.SetObserved(observed)
	narr := io.Discard
	if observed {
		narr = stderr
	}
	// The summary lines are the human form of the result; in JSON mode the one
	// JSON line is the result, and they are only narration.
	summary := stdout
	if inv.Mode == common.ModeJSON {
		summary = narr
	}

	// Configure git hooks before any short-circuit. ./make is the bootstrap
	// entry point — running it once on a fresh clone enables hooks. Idempotent
	// and fast (a single `git config` call), so it's safe to do unconditionally.
	if err := setupHooks(root); err != nil {
		fmt.Fprintf(stderr, "warning: git hooks setup failed: %v\n", err)
	}
	if err := checkIgnored(root); err != nil {
		return err
	}
	tools, err := discoverTools(root)
	if err != nil {
		return err
	}
	if err := checkWorkspaceTools(root, tools); err != nil {
		return err
	}

	hash, err := common.ToolsSourceHash(root)
	if err != nil {
		return fmt.Errorf("computing source hash: %w", err)
	}

	// The up-to-date question is the source hash alone: a new HEAD with the same
	// tools sources builds the same tools, so it is no reason to rebuild.
	binDir := filepath.Join(root, "bin")
	hashFile := filepath.Join(binDir, ".tools.hash")
	upToDate := !inv.Flags["rebuild"] && binUpToDate(binDir, hashFile, hash, tools)

	built := []string{}
	if upToDate {
		fmt.Fprintf(summary, "Tools up to date (%d tools, hash: %s..)\n", len(tools), hash[:12])
	} else {
		if built, err = buildTools(root, binDir, tools, hash, narr, stderr); err != nil {
			return err
		}
		// Write hash sidecar for up-to-date check
		os.WriteFile(hashFile, []byte(hash+"\n"), 0o644)
		// Invalidate gate values — tools changed, prior verify results are stale
		common.InvalidateGateValues(root)
		fmt.Fprintf(summary, "%d tools built\n", len(built))
	}
	fmt.Fprintf(summary, "done (%s)\n", time.Since(start).Round(time.Millisecond))

	if inv.Mode == common.ModeJSON {
		return json.NewEncoder(stdout).Encode(result{UpToDate: upToDate, Built: built, Removed: []string{}})
	}
	return nil
}

// wireHooks is common.RunSetup with its stdout pointed at narration: RunSetup
// announces a change of core.hooksPath on os.Stdout, which here carries only
// the result.
func wireHooks(root string) error {
	saved := os.Stdout
	os.Stdout = common.Narration()
	defer func() { os.Stdout = saved }()
	return common.RunSetup(root)
}

// mustBeIgnored is what ./make and the tools it builds write into the checkout,
// each with the .gitignore line that keeps it out of every commit.
var mustBeIgnored = []struct{ path, line string }{
	{"bin/", "/bin/"},
	{".workspace/", "/.workspace/"},
	{".home/", "/.home/"},
}

// checkIgnored refuses a checkout whose git would pick up what ./make and the
// tools write: building into it would leave binaries and per-clone state one
// `git add -A` away from a commit.
func checkIgnored(root string) error {
	var missing []string
	for _, m := range mustBeIgnored {
		out, err := exec.Command("git", "-C", root, "check-ignore", "-q", "--", m.path).CombinedOutput()
		var exit *exec.ExitError
		switch {
		case err == nil: // exit 0: ignored
		case errors.As(err, &exit) && exit.ExitCode() == 1:
			missing = append(missing, fmt.Sprintf("%s is not ignored: add the line %s to .gitignore", m.path, m.line))
		default:
			msg := strings.TrimSpace(string(out))
			if msg == "" {
				msg = err.Error()
			}
			return fmt.Errorf("cannot tell whether this checkout ignores %s: git check-ignore: %s", m.path, msg)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("will not build: this checkout does not ignore what ./make and its tools write\n  %s", strings.Join(missing, "\n  "))
	}
	return nil
}

// discoverTools lists the cmd/ subdirectories, excluding "make" itself.
func discoverTools(root string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, "tools", "build", "cmd"))
	if err != nil {
		return nil, fmt.Errorf("reading tools/build/cmd: %w", err)
	}
	var tools []string
	for _, e := range entries {
		if e.IsDir() && e.Name() != "make" {
			tools = append(tools, e.Name())
		}
	}
	return tools, nil
}

// workspaceProject is the relative path of the file a workspace setup writes,
// listing the tools it installed into bin/.
const workspaceProject = ".workspace/project.json"

// checkWorkspaceTools refuses to build a tool a workspace setup installed into
// bin/ under the same name: building it would silently replace that binary.
// No project file means no workspace tools.
func checkWorkspaceTools(root string, tools []string) error {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(workspaceProject)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("will not build: cannot read %s to see which tools a setup installed: %w", workspaceProject, err)
	}
	var project struct {
		Tools []string `json:"tools"`
	}
	if err := json.Unmarshal(data, &project); err != nil {
		return fmt.Errorf("will not build: %s cannot be parsed, so which tools a setup installed is unknown: %v", workspaceProject, err)
	}
	var clash []string
	for _, name := range tools {
		if slices.Contains(project.Tools, name) {
			clash = append(clash, name)
		}
	}
	switch len(clash) {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("./make will not build %s: %s lists it as a workspace tool a setup installed", clash[0], workspaceProject)
	default:
		return fmt.Errorf("./make will not build %s: %s lists them as workspace tools a setup installed", strings.Join(clash, ", "), workspaceProject)
	}
}

// binUpToDate reports whether bin/ was built from these sources and still has
// every tool.
func binUpToDate(binDir, hashFile, hash string, tools []string) bool {
	stored, err := os.ReadFile(hashFile)
	if err != nil || strings.TrimSpace(string(stored)) != hash {
		return false
	}
	for _, name := range tools {
		if !common.Exists(filepath.Join(binDir, name+common.ExeSuffix())) {
			return false
		}
	}
	return true
}

// commonPkg is the import path of the package holding the baked-in root, named
// here for the -X linker flag that stamps it.
const commonPkg = "github.com/promise-language/promise/tools/build/common"

// bootstrapRoot returns the repository this bootstrap belongs to, from its own
// source location at compile time.
//
// The tools it builds get their root stamped in with -ldflags, but the
// bootstrap itself cannot: it runs via `go run`, so it has no stamp and no home
// in <root>/bin. runtime.Caller is the remaining build-time fact — `go run`
// compiles this file from the checkout it lives in, so the path is that
// checkout. The alternative would be cwd, which is whatever directory the
// caller last happened to be in (T1814).
func bootstrapRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0) // <root>/tools/build/cmd/make/main.go
	if !ok {
		return "", fmt.Errorf("cannot locate this checkout from the bootstrap's own path")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(file)))))
	if _, err := os.Stat(filepath.Join(root, "catalog.toml")); err != nil {
		return "", fmt.Errorf("derived repo root %s has no catalog.toml (was this built with -trimpath?): %w", root, err)
	}
	return root, nil
}

// buildTools compiles every discovered build tool into bin/ and returns the
// names it built. Progress goes to narr, each failure to stderr; the error names
// the failed count if any tool fails to build.
func buildTools(root, binDir string, tools []string, hash string, narr, stderr io.Writer) ([]string, error) {
	fmt.Fprintf(narr, "Building %d tools (hash: %s..)\n", len(tools), hash[:12])

	// Every tool is built for exactly one repository and carries which one, so
	// it never has to work that out from the environment it runs in.
	// The root is base64-encoded: -ldflags is split on whitespace, so a repo path
	// with a space in it would otherwise break the link, and a path with a quote
	// would break any quoting used to fix that. Both are ordinary paths on
	// Windows and macOS.
	ldflags := fmt.Sprintf("-s -w -X main.sourceHash=%s -X %s.bakedRoot=%s",
		hash, commonPkg, common.EncodeRoot(root))
	built := []string{}
	failed := 0
	for _, name := range tools {
		out := filepath.Join(binDir, name+common.ExeSuffix())
		if err := buildTool(root, name, out, ldflags); err != nil {
			fmt.Fprintf(stderr, "  FAIL %s: %v\n", name, err)
			failed++
			continue
		}
		built = append(built, name)
		if info, err := os.Stat(out); err == nil {
			fmt.Fprintf(narr, "  %-12s %.1f MB\n", name, float64(info.Size())/(1024*1024))
		} else {
			fmt.Fprintf(narr, "  %s\n", name)
		}
	}
	if failed > 0 {
		return built, fmt.Errorf("%d/%d tools failed", failed, len(tools))
	}
	return built, nil
}

// goBuild compiles tools/build/cmd/<name> into out.
func goBuild(root, name, out, ldflags string) error {
	return common.RunIn(
		filepath.Join(root, "tools", "build"),
		"go", "build", "-trimpath",
		"-ldflags", ldflags,
		"-o", out,
		"./cmd/"+name,
	)
}
