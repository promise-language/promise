package common

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// runFixtureRoot is a tree that builds the commands named and has bin/ for
// whatever a test puts there.
func runFixtureRoot(t *testing.T, commands ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, c := range append(commands, metaBuilderName) {
		if err := os.MkdirAll(filepath.Join(root, "tools", "build", "cmd", c), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestRunRun_ACommandWithNoBinaryRefusesUnbuilt: a command this project builds
// whose binary is absent is a refusal, not a failure — the recovery is the
// build — and its own argv is left alone rather than parsed as run's flags.
func TestRunRun_ACommandWithNoBinaryRefusesUnbuilt(t *testing.T) {
	root := runFixtureRoot(t, "format")
	for _, tc := range []struct {
		name     string
		terminal bool
		want     string
	}{
		{"piped", false, `{"condition":"unbuilt","recovery":"./make"}` + "\n"},
		{"at a terminal", true, "run: this project builds format, but bin/format" + ExeSuffix() + " is not there — run ./make\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cliTerminals(t, tc.terminal, false)
			var out bytes.Buffer
			err := RunRun(root, []string{"format", "--bogus", "-json"}, nil, &out)
			if code := ExitCode(err); code != ExitRefused {
				t.Errorf("exit = %d (%v), want %d", code, err, ExitRefused)
			}
			if got := strings.ReplaceAll(out.String(), `.\make.cmd`, "./make"); got != tc.want {
				t.Errorf("stdout = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRunRun_MalformedInvocationsWriteNothing: a bare run, a name that is
// neither a gate nor a command, and a flag the listing does not take are each a
// usage error (exit 2) with stdout empty.
func TestRunRun_MalformedInvocationsWriteNothing(t *testing.T) {
	root := runFixtureRoot(t, "format")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "expecting a subcommand: <gate> | <command>"},
		{[]string{"nosuch", "--verdict"}, `no gate or command named "nosuch"`},
		{[]string{"--list", "--verdict"}, "unknown flag --verdict"},
		{[]string{"fit", "--verdict", "-json"}, "--verdict writes one form only"},
	} {
		var out bytes.Buffer
		err := RunRun(root, tc.args, strings.NewReader(""), &out)
		if code := ExitCode(err); code != 2 || err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("RunRun(%q) = %v (exit %d), want exit 2 saying %q", tc.args, err, code, tc.want)
		}
		if out.Len() != 0 {
			t.Errorf("RunRun(%q) wrote %q on stdout; a usage error writes nothing there", tc.args, out.String())
		}
	}
}

// TestRunOneGate_RelaysTheGatesOwnRefusal: a gate that refuses to measure says
// why on stderr only, so `bin/run <gate>` asks it again where it writes the
// object and relays that object unchanged — an unstamped gate is not reported
// as a stale one. A gate that answers anything else is reported as stale.
func TestRunOneGate_RelaysTheGatesOwnRefusal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in gate is a shell script")
	}
	for _, tc := range []struct {
		name, version, want string
	}{
		{"its own condition", `echo '{"condition":"unstamped","recovery":"./make"}'; exit 3`, `{"condition":"unstamped","recovery":"./make"}`},
		{"fit again by now", `echo '{"project":"promise","text":"x"}'; exit 0`, `{"condition":"stale","recovery":"./make"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := runFixtureRoot(t)
			script := "#!/bin/sh\n" +
				"if [ \"$1\" = -version ]; then " + tc.version + "; fi\n" +
				"echo 'gate: this binary was not built by ./make — run ./make' >&2\nexit 3\n"
			if err := os.WriteFile(filepath.Join(root, "bin", "gate"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			err := runOneGate(root, "fit", ModeJSON, &out)
			if code := ExitCode(err); code != ExitRefused {
				t.Errorf("exit = %d (%v), want %d", code, err, ExitRefused)
			}
			if got := strings.TrimSpace(out.String()); got != tc.want {
				t.Errorf("stdout = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRunList_ANameThatIsBothIsAnError: `bin/run <name>` dispatches commands
// and gates alike, so a command named like a gate would make one of them
// unreachable. The listing refuses rather than describing two things.
func TestRunList_ANameThatIsBothIsAnError(t *testing.T) {
	root := runFixtureRoot(t, "fit", "format")
	var out bytes.Buffer
	err := writeRunList(root, &out, ModeJSON)
	if err == nil || !strings.Contains(err.Error(), "fit") || out.Len() != 0 {
		t.Errorf("err = %v, stdout %q; want the collision named and nothing written", err, out.String())
	}
}

// TestRunOneGate_MeasuresThroughTheGateAndJudges: by hand, bin/run runs
// bin/gate as a process and judges what it printed — in JSON the envelope and
// the verdict together, for a person each metric beside its term — exiting 1
// when the verdict is not acceptable, and 1 with nothing written when the gate
// printed no envelope.
func TestRunOneGate_MeasuresThroughTheGateAndJudges(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in gate is a shell script")
	}
	for _, tc := range []struct {
		name     string
		worktree int64
		mode     OutputMode
		wantCode int
		want     string
	}{
		{"acceptable, in JSON", 50, ModeJSON, 0, `"verdict":{"acceptable":true`},
		{"below the floor, in JSON", 5, ModeJSON, 1, `"verdict":{"acceptable":false`},
		{"below the floor, for a person", 5, ModeHuman, 1, "not acceptable:\n  worktree_free_bytes is 5, cap at_least 10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := writeTermsFiles(t, `{"worktree_free_bytes": {"direction": "at_least", "cap": 10}}`, `{}`)
			writeFile(t, root, "bin/gate", "#!/bin/sh\ncat <<'EOF'\n"+fitEnvelope(tc.worktree)+"\nEOF\n")
			if err := os.Chmod(filepath.Join(root, "bin", "gate"), 0o755); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			err := runOneGate(root, "fit", tc.mode, &out)
			if code := ExitCode(err); code != tc.wantCode {
				t.Errorf("exit = %d (%v), want %d", code, err, tc.wantCode)
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("stdout = %q, want it to hold %q", out.String(), tc.want)
			}
			if tc.mode == ModeJSON && !strings.HasPrefix(out.String(), `{"envelope":{"schema_version":1,"gate":"fit"`) {
				t.Errorf("stdout = %q, want the envelope beside the verdict", out.String())
			}
		})
	}
	t.Run("no envelope", func(t *testing.T) {
		root := writeTermsFiles(t, `{"worktree_free_bytes": {"direction": "at_least", "cap": 10}}`, `{}`)
		writeFile(t, root, "bin/gate", "#!/bin/sh\necho 'gate: fit: free space at /x: no such file' >&2\nexit 1\n")
		if err := os.Chmod(filepath.Join(root, "bin", "gate"), 0o755); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		err := runOneGate(root, "fit", ModeJSON, &out)
		if ExitCode(err) != 1 || err == nil || !strings.Contains(err.Error(), "did not measure anything") || out.Len() != 0 {
			t.Errorf("err = %v, stdout %q; want exit 1, nothing written", err, out.String())
		}
	})
}

// TestRunRun_ACommandsArgvAndStatusAreItsOwn: a command this project builds
// gets its argv verbatim — run's flags included — and its exit status is run's.
func TestRunRun_ACommandsArgvAndStatusAreItsOwn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in command is a shell script")
	}
	root := runFixtureRoot(t, "format")
	writeFile(t, root, "bin/format", "#!/bin/sh\necho \"$@\"\nexit 7\n")
	if err := os.Chmod(filepath.Join(root, "bin", "format"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := RunRun(root, []string{"format", "--check", "-json", "x"}, strings.NewReader(""), &out)
	if code := ExitCode(err); code != 7 {
		t.Errorf("exit = %d (%v), want the command's own 7", code, err)
	}
	if out.String() != "--check -json x\n" {
		t.Errorf("the command saw %q, want its argv verbatim", out.String())
	}
}

// TestRunRun_TheListingAnswersAParameterFile: the listing setup and the sync
// ask for through -json-input is the one --list prints — every command an
// object row, verify declaring the host scope, the gates bare names.
func TestRunRun_TheListingAnswersAParameterFile(t *testing.T) {
	root := runFixtureRoot(t, "format", "verify")
	writeFile(t, root, ".home/tmp/invoke-1.json", `{"list": true}`)
	var out bytes.Buffer
	if err := RunRun(root, []string{"-json", "-json-input", filepath.Join(root, ".home", "tmp", "invoke-1.json")}, nil, &out); err != nil {
		t.Fatalf("RunRun: %v", err)
	}
	var got struct {
		Commands []map[string]string `json:"commands"`
		Gates    []string            `json:"gates"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("not one JSON listing: %v\n%s", err, out.String())
	}
	if want := []map[string]string{{"name": "format"}, {"name": "verify", "serialize": "host"}}; fmt.Sprint(got.Commands) != fmt.Sprint(want) {
		t.Errorf("commands = %v, want %v", got.Commands, want)
	}
	if !slices.Equal(got.Gates, ContractGateNames()) {
		t.Errorf("gates = %v, want %v", got.Gates, ContractGateNames())
	}
}

// TestHelpAndVersion_GateAndRunAnswerTheContractsShapes: each tool's -help
// names the one class of command it answers — never its members — with the
// command that lists them and the flag that class takes, beside its own
// listing flag and the built-ins; -version is the tools source hash the
// staleness check compares, the one ./make computes.
func TestHelpAndVersion_GateAndRunAnswerTheContractsShapes(t *testing.T) {
	root, err := RootForTests()
	if err != nil {
		t.Fatal(err)
	}
	hash, err := ToolsSourceHash(root)
	if err != nil {
		t.Fatal(err)
	}
	type flag struct {
		Name      string   `json:"name"`
		Spellings []string `json:"spellings"`
		Type      string   `json:"type"`
	}
	var doc struct {
		Project  string `json:"project"`
		Flags    []flag `json:"flags"`
		Commands []struct {
			Name         string `json:"name"`
			EnumeratedBy string `json:"enumerated-by"`
			Flags        []flag `json:"flags"`
		} `json:"commands"`
	}
	for _, tc := range []struct {
		tool, class, enumeratedBy, classFlag string
		run                                  func(args ...string) (string, error)
	}{
		{"gate", "<gate>", "gate --list", "envelope", func(args ...string) (string, error) { return runGate(t, root, args...) }},
		{"run", "<gate> | <command>", "run --list", "verdict", func(args ...string) (string, error) {
			var out bytes.Buffer
			err := RunRun(root, args, nil, &out)
			return out.String(), err
		}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			out, err := tc.run("-help", "-json")
			if err != nil || json.Unmarshal([]byte(out), &doc) != nil {
				t.Fatalf("-help -json = %q, %v", out, err)
			}
			flags := map[string]flag{}
			for _, f := range doc.Flags {
				flags[f.Name] = f
			}
			if doc.Project != "promise" ||
				!slices.Equal(flags["list"].Spellings, []string{"list", "no-list"}) ||
				!slices.Equal(flags["output-mode"].Spellings, []string{"json", "human"}) || flags["output-mode"].Type != "enumeration" ||
				flags["json-input"].Type != "path" || flags["narrate"].Spellings != nil || flags["version"].Spellings != nil {
				t.Errorf("%s -help flags = %+v", tc.tool, doc.Flags)
			}
			if len(doc.Commands) != 1 || doc.Commands[0].Name != tc.class || doc.Commands[0].EnumeratedBy != tc.enumeratedBy ||
				len(doc.Commands[0].Flags) != 1 || doc.Commands[0].Flags[0].Name != tc.classFlag {
				t.Errorf("%s -help commands = %+v, want the one class %q listed by %q with -%s", tc.tool, doc.Commands, tc.class, tc.enumeratedBy, tc.classFlag)
			}
			if strings.Contains(out, "tested:go") {
				t.Errorf("%s -help names a gate; help points at the listing instead: %s", tc.tool, out)
			}
			out, err = tc.run("-version", "-json")
			if want := `{"project":"promise","text":"` + hash + `"}` + "\n"; err != nil || out != want {
				t.Errorf("%s -version -json = %q, %v; want %q", tc.tool, out, err, want)
			}
		})
	}
}
