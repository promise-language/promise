package common

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestFitness_EachUnfitBinaryRefusesWithItsCondition: a binary stamped from the
// tree in front of it may act; one go build made, one stamped from another tools
// tree, and one whose stamped repository is gone each refuse, naming why, with
// ./make as the recovery.
func TestFitness_EachUnfitBinaryRefusesWithItsCondition(t *testing.T) {
	repo, err := RootForTests()
	if err != nil {
		t.Fatalf("RootForTests: %v", err)
	}
	hash, err := ToolsSourceHash(repo)
	if err != nil {
		t.Fatalf("ToolsSourceHash: %v", err)
	}
	withBakedRoot(t, repo)
	if r := fitness(hash); r != nil {
		t.Fatalf("fitness(current hash) = %+v, want nil", r)
	}
	_, relative := MakeCommands(repo)
	for _, tc := range []struct {
		name, hash, root, want string
	}{
		{"built by go build", "dev", repo, RefusalUnstamped},
		{"carrying no stamp", "", repo, RefusalUnstamped},
		{"stamped from another tree", "not-the-hash-of-any-real-tools-tree", repo, RefusalStale},
		{"its repository gone", hash, filepath.Join(t.TempDir(), "gone"), RefusalRepositoryUnreachable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withBakedRoot(t, tc.root)
			r := fitness(tc.hash)
			if r == nil || r.Condition != tc.want || r.Recovery != relative {
				t.Errorf("fitness(%q) = %+v, want condition %q, recovery %q", tc.hash, r, tc.want, relative)
			}
		})
	}
}

// TestWriteRefusal_WhereTheRefusalGoes: the refusal is the result, so it goes
// where the mode in force puts one — the {condition, recovery} object, and
// nothing else, in JSON; one line for a person — except in a protocol mode
// spelled on the command line, where stdout belongs to an envelope or a verdict
// and stays empty while stderr carries the line. Every path exits 3.
func TestWriteRefusal_WhereTheRefusalGoes(t *testing.T) {
	r := &Refusal{Condition: RefusalStale, Recovery: "./make", Detail: "the tools source has changed since this binary was built"}
	const object = `{"condition":"stale","recovery":"./make"}` + "\n"
	const line = "gate: the tools source has changed since this binary was built — run ./make\n"
	for _, tc := range []struct {
		name                 string
		args                 []string
		terminal             bool
		wantStdout, wantErrs string
	}{
		{"piped, nothing forced", []string{"--list"}, false, object, ""},
		{"at a terminal", []string{"--list"}, true, line, ""},
		{"-json at a terminal", []string{"--list", "-json"}, true, object, ""},
		{"--human piped", []string{"--list", "--human"}, false, line, ""},
		{"-help", []string{"-help"}, false, object, ""},
		{"--envelope", []string{"fit", "--envelope"}, false, "", line},
		{"-envelope at a terminal", []string{"fit", "-envelope"}, true, "", line},
		{"envelope after --", []string{"fit", "--", "--envelope"}, false, object, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cliTerminals(t, tc.terminal, false)
			var stdout, stderr bytes.Buffer
			if code := writeRefusal(&stdout, &stderr, "gate", r, tc.args, []string{"envelope"}); code != ExitRefused {
				t.Errorf("exit = %d, want %d", code, ExitRefused)
			}
			if stdout.String() != tc.wantStdout || stderr.String() != tc.wantErrs {
				t.Errorf("stdout %q, stderr %q; want stdout %q, stderr %q", stdout.String(), stderr.String(), tc.wantStdout, tc.wantErrs)
			}
		})
	}
}

// TestMakeCommands pins what the two spellings mean, because the staleness
// messages promise it in words: the first resolves from any working directory,
// the second only at the repo root. A message that got these backwards is what
// wedged a session in T1813.
func TestMakeCommands(t *testing.T) {
	base := "make"
	wantRelative := "./make"
	if runtime.GOOS == "windows" {
		base = "make.cmd"
		wantRelative = `.\make.cmd`
	}

	// The fixture roots stand in for a real checkout, so they have to be
	// absolute the way the platform means it — the first assertion below is
	// about the path MakeCommands returns, and a fixture that fails it teaches
	// nothing. On Windows that needs a volume: filepath.Join(`\`, …) yields
	// `\srv\promise`, which is rooted but drive-relative, so filepath.IsAbs
	// rejects it and the test fails on its own input.
	volume := string(filepath.Separator)
	if runtime.GOOS == "windows" {
		volume = `C:\`
	}

	// A path with a space is an ordinary checkout (/Users/John Doe/promise),
	// so the spellings have to survive one without quoting or splitting.
	for _, root := range []string{
		filepath.Join(volume, "srv", "promise"),
		filepath.Join(volume, "Users", "John Doe", "promise"),
	} {
		abs, relative := MakeCommands(root)

		if !filepath.IsAbs(abs) {
			t.Errorf("MakeCommands(%q) abs = %q, which does not resolve from an arbitrary cwd", root, abs)
		}
		if want := filepath.Join(root, base); abs != want {
			t.Errorf("MakeCommands(%q) abs = %q, want %q", root, abs, want)
		}
		if relative != wantRelative {
			t.Errorf("MakeCommands(%q) relative = %q, want %q", root, relative, wantRelative)
		}
		// Both spellings must name the same script — one being the other's
		// root-relative form is the whole reason a caller can pick either.
		if filepath.Base(abs) != base || !strings.HasSuffix(relative, base) {
			t.Errorf("MakeCommands(%q) = (%q, %q): the two spellings name different scripts", root, abs, relative)
		}
	}
}

// TestCheckStaleAcceptsCurrentHash: a matching hash returns rather than exiting.
// It is the path every tool takes on every ordinary invocation, so a regression
// here would exit(1) out of the whole toolchain.
func TestCheckStaleAcceptsCurrentHash(t *testing.T) {
	root, err := RootForTests()
	if err != nil {
		t.Fatalf("RootForTests: %v", err)
	}
	withBakedRoot(t, root)

	hash, err := ToolsSourceHash(root)
	if err != nil {
		t.Fatalf("ToolsSourceHash: %v", err)
	}
	CheckStale(hash) // must not exit
}

// TestCheckStaleDevHashSkips: "dev" means `go run`, where there is no compiled
// hash to compare and no root to resolve.
func TestCheckStaleDevHashSkips(t *testing.T) {
	withBakedRoot(t, filepath.Join(t.TempDir(), "not-a-repo"))
	CheckStale("dev") // must not exit, and must not even look for a root
}

// checkStaleHelperEnv names the mode a helper subprocess runs in; empty means
// "this is the ordinary test binary, skip".
const checkStaleHelperEnv = "PROMISE_TEST_CHECKSTALE_MODE"

// TestCheckStaleHelperProcess is not a test — it is the body of the subprocess
// TestCheckStaleExitPaths spawns. CheckStale's failure paths all end in
// os.Exit, which cannot be observed in-process.
func TestCheckStaleHelperProcess(t *testing.T) {
	if os.Getenv(checkStaleHelperEnv) == "" {
		t.Skip("helper process; driven by TestCheckStaleExitPaths")
	}
	withBakedRoot(t, os.Getenv("PROMISE_TEST_CHECKSTALE_ROOT"))
	CheckStale("not-the-hash-of-any-real-tools-tree")
}

// TestCheckStaleExitPaths covers the three ways CheckStale ends a tool run, and
// asserts the one thing T1813 showed to be load-bearing: the message names a
// rebuild command that resolves from wherever the caller happens to be, not
// only from the repo root.
func TestCheckStaleExitPaths(t *testing.T) {
	repo, err := RootForTests()
	if err != nil {
		t.Fatalf("RootForTests: %v", err)
	}
	absMake, relativeMake := MakeCommands(repo)

	// A directory that looks like a repo but holds no tools tree — the shape of
	// the T1813 scratch dir, reached here through the stamp rather than a cwd
	// walk, since a cwd walk can no longer reach it at all.
	emptyRepo := t.TempDir()
	if err := os.WriteFile(filepath.Join(emptyRepo, "catalog.toml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		root    string
		want    []string
		notWant []string
	}{
		{
			name: "stale tools name a cwd-independent rebuild",
			root: repo,
			// Both spellings, and the label saying where the bare one
			// applies: the relative form alone is exactly the suggestion a
			// drifted cwd cannot act on.
			want: []string{"tools source has changed", absMake, relativeMake, "from the repo root"},
		},
		{
			name:    "root no longer a repo",
			root:    filepath.Join(t.TempDir(), "gone"),
			want:    []string{"error:", "no longer looks like a Promise repo"},
			notWant: []string{"tools source has changed"},
		},
		{
			name:    "root has no tools tree",
			root:    emptyRepo,
			want:    []string{"error:", "walk tools/build"},
			notWant: []string{"tools source has changed"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestCheckStaleHelperProcess$", "-test.v")
			cmd.Env = append(os.Environ(),
				checkStaleHelperEnv+"=1",
				"PROMISE_TEST_CHECKSTALE_ROOT="+tt.root,
			)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("expected CheckStale to exit non-zero; output:\n%s", out)
			}
			got := string(out)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("CheckStale output does not mention %q:\n%s", want, got)
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(got, notWant) {
					t.Errorf("CheckStale output unexpectedly mentions %q:\n%s", notWant, got)
				}
			}
		})
	}
}
