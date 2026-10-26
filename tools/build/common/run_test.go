package common

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
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
