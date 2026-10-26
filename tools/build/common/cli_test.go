package common

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The specs below are shaped like the tools that will declare them: a root
// listing flag and a command class with a flag whose output its contract fixes.

func cliGateSpec() ToolSpec {
	return ToolSpec{
		Tool:        "gate",
		Description: "measure this project's gates",
		Flags:       []FlagSpec{{Name: "list", Type: FlagBoolean, Description: "print the gates this project provides"}},
		Commands: []CommandSpec{{
			Name: "<gate>", Description: "a gate this project answers", EnumeratedBy: "gate --list",
			Flags: []FlagSpec{{Name: "envelope", Type: FlagBoolean, Description: "write the flow envelope", FixesOutput: true}},
		}},
	}
}

func cliRunSpec() ToolSpec {
	return ToolSpec{
		Tool:        "run",
		Description: "judge this project's gates",
		Flags:       []FlagSpec{{Name: "list", Type: FlagBoolean, Description: "print what run answers"}},
		Commands: []CommandSpec{{
			Name: "<gate> | <command>", Description: "a gate to judge or a command to run", EnumeratedBy: "run --list",
			Flags: []FlagSpec{{Name: "verdict", Type: FlagBoolean, Description: "judge an envelope on stdin", FixesOutput: true}},
		}},
		PassThrough: func(c string) bool { return c == "fmt" },
	}
}

func cliVerifySpec() ToolSpec {
	return ToolSpec{Tool: "verify", Description: "build and test what a change touches"}
}

// cliTerminals decides what stdout and stderr are for the rest of the test.
func cliTerminals(t *testing.T, stdout, stderr bool) {
	t.Helper()
	so, se := stdoutIsTerminal, stderrIsTerminal
	stdoutIsTerminal = func() bool { return stdout }
	stderrIsTerminal = func() bool { return stderr }
	t.Cleanup(func() { stdoutIsTerminal, stderrIsTerminal = so, se })
}

func cliSameInvocation(got, want Invocation) bool {
	return got.Command == want.Command &&
		maps.Equal(got.Flags, want.Flags) &&
		slices.Equal(got.Args, want.Args) &&
		got.Mode == want.Mode && got.ModeForced == want.ModeForced &&
		got.Narrate == want.Narrate && got.Help == want.Help && got.Version == want.Version
}

// cliCheckProblems asserts err is a *UsageError carrying exactly len(want)
// problems, each containing its want entry, every line prefixed "<tool>: ".
func cliCheckProblems(t *testing.T, tool string, err error, want []string) {
	t.Helper()
	var ue *UsageError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v (%T), want *UsageError", err, err)
	}
	if len(ue.Problems) != len(want) {
		t.Errorf("got %d problems, want %d:\n%s", len(ue.Problems), len(want), ue.Error())
	}
	for _, w := range want {
		found := false
		for _, p := range ue.Problems {
			found = found || strings.Contains(p, w)
		}
		if !found {
			t.Errorf("no problem contains %q:\n%s", w, ue.Error())
		}
	}
	for _, line := range strings.Split(ue.Error(), "\n") {
		if !strings.HasPrefix(line, tool+": ") {
			t.Errorf("line %q is not prefixed %q", line, tool+": ")
		}
	}
	if ExitCode(err) != 2 {
		t.Errorf("ExitCode = %d, want 2", ExitCode(err))
	}
}

func TestCLIParseInvocationAccepts(t *testing.T) {
	cliTerminals(t, false, false)
	gate, run, verify := cliGateSpec(), cliRunSpec(), cliVerifySpec()
	for _, tc := range []struct {
		name string
		spec ToolSpec
		argv []string
		want Invocation
	}{
		{"bare", gate, nil, Invocation{}},
		{"single dash", gate, []string{"-list"}, Invocation{Flags: map[string]bool{"list": true}}},
		{"double dash", gate, []string{"--list"}, Invocation{Flags: map[string]bool{"list": true}}},
		{"denial", gate, []string{"-no-list"}, Invocation{Flags: map[string]bool{"list": false}}},
		{"command then flag", gate, []string{"tested:go", "--envelope"},
			Invocation{Command: "tested:go", Flags: map[string]bool{"envelope": true}}},
		{"command then denial", gate, []string{"tested:go", "-no-envelope"},
			Invocation{Command: "tested:go", Flags: map[string]bool{"envelope": false}}},
		{"denied fixed-output flag leaves the mode free", gate, []string{"tested:go", "-no-envelope", "-human"},
			Invocation{Command: "tested:go", Flags: map[string]bool{"envelope": false}, Mode: ModeHuman, ModeForced: true}},
		{"-json forces", gate, []string{"-list", "-json"},
			Invocation{Flags: map[string]bool{"list": true}, Mode: ModeJSON, ModeForced: true}},
		{"--human forces", gate, []string{"-list", "--human"},
			Invocation{Flags: map[string]bool{"list": true}, Mode: ModeHuman, ModeForced: true}},
		{"narrate", verify, []string{"-narrate"}, Invocation{Narrate: true}},
		{"help alone", gate, []string{"-help"}, Invocation{Help: true}},
		{"help -json", gate, []string{"-help", "-json"}, Invocation{Help: true, ModeForced: true}},
		{"version alone", gate, []string{"--version"}, Invocation{Version: true}},
		{"version -human", gate, []string{"-version", "-human"}, Invocation{Version: true, Mode: ModeHuman, ModeForced: true}},
		{"-- then the command", gate, []string{"--", "tested:go"}, Invocation{Command: "tested:go"}},
		{"-- alone", gate, []string{"--"}, Invocation{}},
		{"pass-through keeps flags verbatim", run, []string{"fmt", "-json", "--", "x", "-help", "-no-list"},
			Invocation{Command: "fmt", Args: []string{"-json", "--", "x", "-help", "-no-list"}}},
		{"pass-through without args", run, []string{"fmt"}, Invocation{Command: "fmt"}},
		{"pass-through after --", run, []string{"--", "fmt", "-json"}, Invocation{Command: "fmt", Args: []string{"-json"}}},
		{"run's class flag", run, []string{"tested:go", "-verdict"},
			Invocation{Command: "tested:go", Flags: map[string]bool{"verdict": true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseInvocation(tc.spec, tc.argv)
			if err != nil {
				t.Fatalf("ParseInvocation(%q): %v", tc.argv, err)
			}
			if !cliSameInvocation(got, tc.want) {
				t.Errorf("ParseInvocation(%q) = %+v, want %+v", tc.argv, got, tc.want)
			}
		})
	}
}

func TestCLIParseInvocationRefuses(t *testing.T) {
	cliTerminals(t, false, false)
	gate, run, verify := cliGateSpec(), cliRunSpec(), cliVerifySpec()
	const helpPointer = "; run `gate -help` for every flag"
	for _, tc := range []struct {
		name string
		spec ToolSpec
		argv []string
		want []string
	}{
		{"-h is unknown", gate, []string{"-h"}, []string{"unknown flag -h; the closest is -help" + helpPointer}},
		{"closest flag", gate, []string{"-lsit"}, []string{"unknown flag -lsit; the closest is -list" + helpPointer}},
		{"quoted as typed", gate, []string{"--lsit"}, []string{"unknown flag --lsit; the closest is -list" + helpPointer}},
		{"nothing close", gate, []string{"-zzzzzzzz"}, []string{"unknown flag -zzzzzzzz" + helpPointer}},
		{"-flag:value", gate, []string{"-list:true"}, []string{"unknown flag -list:true; the closest is -list" + helpPointer}},
		{"no -no- for a built-in", verify, []string{"-no-narrate"},
			[]string{"unknown flag -no-narrate; the closest is -narrate; run `verify -help` for every flag"}},
		{"case is a different name", gate, []string{"-List"}, []string{"unknown flag -List; the closest is -list"}},
		{"boolean =true", gate, []string{"-list=true"}, []string{"-list takes no value: a boolean is -list or -no-list"}},
		{"denial =false", gate, []string{"--no-list=false"}, []string{"--no-list takes no value: a boolean is -list or -no-list"}},
		{"built-in given a value", gate, []string{"-help=yes"}, []string{"-help takes no value"}},
		{"output mode given a value", gate, []string{"-json=true"}, []string{"-json takes no value: give -json or -human"}},
		{"two problems in one error", gate, []string{"-bogus", "-list=true"},
			[]string{"unknown flag -bogus", "-list takes no value"}},
		{"flag twice", gate, []string{"-list", "-list", "-list"}, []string{"-list is given twice"}},
		{"flag twice across prefixes", gate, []string{"-list", "--list"}, []string{"-list is given twice"}},
		{"asserted then denied", gate, []string{"-list", "-no-list"}, []string{"-list is given twice, as -list and as -no-list"}},
		{"-json twice", gate, []string{"-json", "-json"}, []string{"-json is given twice"}},
		{"-json with -human", gate, []string{"-json", "-human"}, []string{"-json and -human choose one output mode; give one"}},
		{"positional after a flag", gate, []string{"-list", "tested:go"},
			[]string{`unexpected argument "tested:go": gate takes its command first and no other positional`}},
		{"second positional", gate, []string{"tested:go", "other"}, []string{`unexpected argument "other"`}},
		{"flag after a stray positional is still read", gate, []string{"tested:go", "other", "-bogus"},
			[]string{`unexpected argument "other"`, "unknown flag -bogus"}},
		{"-- after the command", gate, []string{"tested:go", "--", "-envelope"}, []string{`unexpected argument "-envelope"`}},
		{"-- after a flag", gate, []string{"-list", "--", "tested:go"}, []string{`unexpected argument "tested:go"`}},
		{"root flag after a command", gate, []string{"tested:go", "-list"}, []string{"unknown flag -list"}},
		{"class flag without a command", gate, []string{"-envelope"}, []string{"unknown flag -envelope"}},
		{"no commands at all", verify, []string{"x"},
			[]string{`unexpected argument "x": verify takes no command and no positional`}},
		{"empty command", gate, []string{""}, []string{`unexpected argument ""`}},
		{"-json-input without a path", gate, []string{"-json-input"}, []string{"-json-input needs a path"}},
		{"-json-input= without a path", gate, []string{"-json-input="}, []string{"-json-input needs a path: -json-input <path>"}},
		{"-help beside a flag", gate, []string{"-help", "-list"}, []string{"-help takes nothing beside it but -json or -human"}},
		{"-help beside a command", gate, []string{"tested:go", "-help"}, []string{"-help takes nothing beside it but -json or -human"}},
		{"-help with -version", gate, []string{"-help", "-version"}, []string{"-help takes nothing beside it but -json or -human"}},
		{"-help beside -narrate", gate, []string{"-help", "-narrate"}, []string{"-help takes nothing beside it"}},
		{"-version beside a flag", gate, []string{"-version", "-no-list"}, []string{"-version takes nothing beside it but -json or -human"}},
		{"-version beside a command", gate, []string{"tested:go", "-version"}, []string{"-version takes nothing beside it"}},
		{"fixed output and -json", gate, []string{"tested:go", "--envelope", "-json"},
			[]string{"--envelope writes one form only, fixed by its contract; drop -json"}},
		{"fixed output and -human", run, []string{"tested:go", "-verdict", "--human"},
			[]string{"-verdict writes one form only, fixed by its contract; drop --human"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseInvocation(tc.spec, tc.argv)
			cliCheckProblems(t, tc.spec.Tool, err, tc.want)
			if !cliSameInvocation(got, Invocation{}) {
				t.Errorf("a refused invocation returned %+v", got)
			}
		})
	}
}

func TestCLIJSONInput(t *testing.T) {
	cliTerminals(t, true, false) // so a forced JSON mode is distinguishable from the default
	gate, run := cliGateSpec(), cliRunSpec()
	human := ModeHuman
	for _, tc := range []struct {
		name string
		spec ToolSpec
		file string   // written to IN
		argv []string // "IN" is replaced with the file's path
		want *Invocation
		bad  []string // problems; "IN" is replaced with the path
	}{
		{name: "a flag", spec: gate, file: `{"list":true}`, argv: []string{"-json-input", "IN"},
			want: &Invocation{Flags: map[string]bool{"list": true}, Mode: human}},
		{name: "false is the denial", spec: gate, file: `{"list":false}`, argv: []string{"-json-input=IN"},
			want: &Invocation{Flags: map[string]bool{"list": false}, Mode: human}},
		{name: "the command and its flag", spec: gate, file: `{"envelope":true,"args":["tested:go"]}`, argv: []string{"-json-input", "IN"},
			want: &Invocation{Command: "tested:go", Flags: map[string]bool{"envelope": true}, Mode: human}},
		{name: "a command in the file makes its flags apply on the line", spec: gate, file: `{"args":["tested:go"]}`,
			argv: []string{"-json-input", "IN", "-no-envelope"},
			want: &Invocation{Command: "tested:go", Flags: map[string]bool{"envelope": false}, Mode: human}},
		{name: "output-mode", spec: gate, file: `{"output-mode":"json"}`, argv: []string{"--json-input", "IN"},
			want: &Invocation{Mode: ModeJSON, ModeForced: true}},
		{name: "built-ins", spec: gate, file: `{"help":true,"output-mode":"human"}`, argv: []string{"-json-input", "IN"},
			want: &Invocation{Help: true, Mode: human, ModeForced: true}},
		{name: "empty args", spec: gate, file: `{"args":[],"narrate":true}`, argv: []string{"-json-input", "IN"},
			want: &Invocation{Narrate: true, Mode: human}},
		{name: "pass-through args", spec: run, file: `{"args":["fmt","-json","x"]}`, argv: []string{"-json-input", "IN"},
			want: &Invocation{Command: "fmt", Args: []string{"-json", "x"}, Mode: human}},

		{name: "unknown key", spec: gate, file: `{"bogus":true}`, argv: []string{"-json-input", "IN"},
			bad: []string{`unknown flag "bogus" in IN; run ` + "`gate -help` for every flag"}},
		{name: "unknown key, with the closest", spec: gate, file: `{"lsit":true}`, argv: []string{"-json-input", "IN"},
			bad: []string{`unknown flag "lsit" in IN; the closest is "list"`}},
		{name: "json-input is not a key", spec: gate, file: `{"json-input":"x"}`, argv: []string{"-json-input", "IN"},
			bad: []string{`unknown flag "json-input" in IN`}},
		{name: "a spelling is not a key", spec: gate, file: `{"json":true}`, argv: []string{"-json-input", "IN"},
			bad: []string{`unknown flag "json" in IN`}},
		{name: "a class flag without a command", spec: gate, file: `{"envelope":true}`, argv: []string{"-json-input", "IN"},
			bad: []string{`unknown flag "envelope" in IN`}},
		{name: "flag in both", spec: gate, file: `{"list":true}`, argv: []string{"-json-input", "IN", "-list"},
			bad: []string{"-list is given both in IN and on the command line"}},
		{name: "denial in both", spec: gate, file: `{"list":false}`, argv: []string{"-no-list", "-json-input", "IN"},
			bad: []string{"-no-list is given both in IN and on the command line"}},
		{name: "output mode in both", spec: gate, file: `{"output-mode":"human"}`, argv: []string{"-json-input", "IN", "-json"},
			bad: []string{"-json is given both in IN and on the command line"}},
		{name: "command in both", spec: gate, file: `{"args":["tested:go"]}`, argv: []string{"tested:go", "-json-input", "IN"},
			bad: []string{"the command is given both in IN and on the command line"}},
		{name: "key twice", spec: gate, file: `{"list":true,"list":false}`, argv: []string{"-json-input", "IN"},
			bad: []string{`"list" is given twice in IN`}},
		{name: "-json-input twice", spec: gate, file: `{}`, argv: []string{"-json-input", "IN", "-json-input", "IN"},
			bad: []string{"-json-input is given twice"}},
		{name: "malformed", spec: gate, file: `{"list": tru`, argv: []string{"-json-input", "IN"},
			bad: []string{"-json-input IN is not valid JSON"}},
		{name: "an array", spec: gate, file: `[{"list":true}]`, argv: []string{"-json-input", "IN"},
			bad: []string{"-json-input IN must hold one JSON object"}},
		{name: "empty", spec: gate, file: ``, argv: []string{"-json-input", "IN"},
			bad: []string{"-json-input IN must hold one JSON object"}},
		{name: "two objects", spec: gate, file: `{} {}`, argv: []string{"-json-input", "IN"},
			bad: []string{"-json-input IN must hold one JSON object"}},
		{name: "flag of the wrong type", spec: gate, file: `{"list":1}`, argv: []string{"-json-input", "IN"},
			bad: []string{`-json-input IN: "list" must be true or false`}},
		{name: "args of the wrong type", spec: gate, file: `{"args":"tested:go"}`, argv: []string{"-json-input", "IN"},
			bad: []string{`-json-input IN: "args" must be an array of strings`}},
		{name: "output-mode out of range", spec: gate, file: `{"output-mode":"xml"}`, argv: []string{"-json-input", "IN"},
			bad: []string{`-json-input IN: "output-mode" must be "json" or "human"`}},
		{name: "a built-in has no false", spec: gate, file: `{"narrate":false}`, argv: []string{"-json-input", "IN"},
			bad: []string{`"narrate" can only be true`}},
		{name: "a second positional in args", spec: gate, file: `{"args":["tested:go","x"]}`, argv: []string{"-json-input", "IN"},
			bad: []string{`unexpected argument "x" in IN`}},
		{name: "a pass-through command takes no flags", spec: run, file: `{"args":["fmt"],"output-mode":"json"}`, argv: []string{"-json-input", "IN"},
			bad: []string{`-json does not apply to "fmt"`}},
		{name: "fixed output from the file", spec: gate, file: `{"args":["tested:go"],"envelope":true,"output-mode":"json"}`,
			argv: []string{"-json-input", "IN"}, bad: []string{"-envelope writes one form only, fixed by its contract; drop -json"}},
		{name: "-help beside a file flag", spec: gate, file: `{"help":true,"list":true}`, argv: []string{"-json-input", "IN"},
			bad: []string{"-help takes nothing beside it but -json or -human"}},
		{name: "problems from both sources", spec: gate, file: `{"bogus":true}`, argv: []string{"-json-input", "IN", "-nope"},
			bad: []string{"unknown flag -nope", `unknown flag "bogus" in IN`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "in.json")
			if err := os.WriteFile(path, []byte(tc.file), 0o644); err != nil {
				t.Fatal(err)
			}
			argv := make([]string, len(tc.argv))
			for i, a := range tc.argv {
				argv[i] = strings.ReplaceAll(a, "IN", path)
			}
			got, err := ParseInvocation(tc.spec, argv)
			if tc.want != nil {
				if err != nil {
					t.Fatalf("ParseInvocation(%q): %v", argv, err)
				}
				if !cliSameInvocation(got, *tc.want) {
					t.Errorf("ParseInvocation(%q) = %+v, want %+v", argv, got, *tc.want)
				}
				return
			}
			bad := make([]string, len(tc.bad))
			for i, b := range tc.bad {
				bad[i] = strings.ReplaceAll(b, "IN", path)
			}
			cliCheckProblems(t, tc.spec.Tool, err, bad)
		})
	}
}

// TestCLIJSONInputRelativePath: the path is the caller's, so it resolves
// against the working directory, and messages name it as the caller wrote it.
func TestCLIJSONInputRelativePath(t *testing.T) {
	cliTerminals(t, false, false)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "args.json"), []byte(`{"list":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	got, err := ParseInvocation(cliGateSpec(), []string{"-json-input", "args.json"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Flags["list"] {
		t.Errorf("Flags = %v, want list asserted", got.Flags)
	}
	_, err = ParseInvocation(cliGateSpec(), []string{"-json-input", "missing.json"})
	cliCheckProblems(t, "gate", err, []string{"-json-input missing.json cannot be read: "})
}

func TestCLIModeFollowsStdoutOnly(t *testing.T) {
	for _, tc := range []struct {
		stdout, stderr bool
		want           OutputMode
	}{
		{true, false, ModeHuman},
		{false, true, ModeJSON},
		{false, false, ModeJSON},
	} {
		t.Run(fmt.Sprintf("stdout=%v,stderr=%v", tc.stdout, tc.stderr), func(t *testing.T) {
			cliTerminals(t, tc.stdout, tc.stderr)
			got, err := ParseInvocation(cliVerifySpec(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if got.Mode != tc.want || got.ModeForced {
				t.Errorf("Mode = %v (forced %v), want %v unforced", got.Mode, got.ModeForced, tc.want)
			}
		})
	}
}

func TestCLIObservedFor(t *testing.T) {
	for _, tc := range []struct {
		stderr, narrate, want bool
	}{
		{true, false, true},
		{false, true, true},
		{false, false, false},
	} {
		cliTerminals(t, false, tc.stderr)
		if got := ObservedFor(Invocation{Narrate: tc.narrate}); got != tc.want {
			t.Errorf("ObservedFor(narrate=%v) with stderr terminal=%v = %v, want %v", tc.narrate, tc.stderr, got, tc.want)
		}
	}
}

func TestCLIExitCode(t *testing.T) {
	usage := &UsageError{Tool: "gate", Problems: []string{"x"}}
	for _, tc := range []struct {
		err  error
		want int
	}{
		{nil, 0},
		{usage, 2},
		{fmt.Errorf("parsing: %w", usage), 2},
		{errors.New("could not measure"), 1},
	} {
		if got := ExitCode(tc.err); got != tc.want {
			t.Errorf("ExitCode(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}

// TestCLIFail: everything goes to stderr, nothing to stdout, and the code is
// the error's.
func TestCLIFail(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		wantStderr string
		wantCode   int
	}{
		{"usage", &UsageError{Tool: "gate", Problems: []string{"unknown flag -x", "-list is given twice"}},
			"gate: unknown flag -x\ngate: -list is given twice\n", 2},
		{"raw usage", BareInvocation(cliGateSpec(), "abc"), BareInvocation(cliGateSpec(), "abc").Error() + "\n", 2},
		{"other", errors.New("could not measure\n"), "gate: could not measure\n", 1},
		{"nil", nil, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var code int
			var stderr string
			stdout := captureStdout(t, func() {
				stderr = captureStderr(t, func() { code = Fail("gate", tc.err) })
			})
			if stdout != "" {
				t.Errorf("Fail wrote %q on stdout", stdout)
			}
			if stderr != tc.wantStderr {
				t.Errorf("stderr = %q, want %q", stderr, tc.wantStderr)
			}
			if code != tc.wantCode {
				t.Errorf("Fail returned %d, want %d", code, tc.wantCode)
			}
		})
	}
}

// cliWrites counts Write calls: a JSON line written in pieces can interleave
// with another writer's.
type cliWrites struct {
	bytes.Buffer
	n int
}

func (w *cliWrites) Write(p []byte) (int, error) { w.n++; return w.Buffer.Write(p) }

func TestCLIWriteHelpJSON(t *testing.T) {
	var w cliWrites
	if err := WriteHelp(&w, cliGateSpec(), ModeJSON); err != nil {
		t.Fatal(err)
	}
	raw := w.String()
	if w.n != 1 || !strings.HasSuffix(raw, "}\n") || strings.Count(raw, "\n") != 1 {
		t.Errorf("help is not one line in one write (%d writes): %q", w.n, raw)
	}
	if !strings.Contains(raw, `"<gate>"`) {
		t.Errorf("the class pattern is escaped: %s", raw)
	}
	var doc struct {
		Project     string                       `json:"project"`
		Description string                       `json:"description"`
		Flags       []map[string]any             `json:"flags"`
		Commands    []map[string]json.RawMessage `json:"commands"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	if doc.Project != "promise" || doc.Description != "measure this project's gates" {
		t.Errorf("project/description = %q/%q", doc.Project, doc.Description)
	}
	var names []string
	for _, f := range doc.Flags {
		names = append(names, f["name"].(string))
	}
	if want := []string{"list", "output-mode", "narrate", "version", "help", "json-input"}; !slices.Equal(names, want) {
		t.Errorf("flags = %v, want %v", names, want)
	}
	want := map[string]struct {
		typ       string
		spellings []any
	}{
		"list":        {"boolean", []any{"list", "no-list"}},
		"output-mode": {"enumeration", []any{"json", "human"}},
		"narrate":     {"boolean", nil},
		"version":     {"boolean", nil},
		"help":        {"boolean", nil},
		"json-input":  {"path", nil},
	}
	for _, f := range doc.Flags {
		name := f["name"].(string)
		if f["type"] != want[name].typ {
			t.Errorf("%s type = %v, want %s", name, f["type"], want[name].typ)
		}
		sp, has := f["spellings"]
		if want[name].spellings == nil {
			if has {
				t.Errorf("%s carries spellings %v; a built-in has one spelling", name, sp)
			}
		} else if got, _ := sp.([]any); !slices.Equal(got, want[name].spellings) {
			t.Errorf("%s spellings = %v, want %v", name, sp, want[name].spellings)
		}
		if d, _ := f["description"].(string); d == "" {
			t.Errorf("%s has no description", name)
		}
	}
	if len(doc.Commands) != 1 {
		t.Fatalf("commands = %v, want one class", doc.Commands)
	}
	c := doc.Commands[0]
	for key, want := range map[string]string{
		"name":          `"<gate>"`,
		"description":   `"a gate this project answers"`,
		"enumerated-by": `"gate --list"`,
		"flags":         `[{"name":"envelope","spellings":["envelope","no-envelope"],"type":"boolean","description":"write the flow envelope"}]`,
	} {
		if string(c[key]) != want {
			t.Errorf("commands[0].%s = %s, want %s", key, c[key], want)
		}
	}

	var none bytes.Buffer
	if err := WriteHelp(&none, cliVerifySpec(), ModeJSON); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(none.String(), `"commands":[]`) {
		t.Errorf("a tool without commands must say so with []: %s", none.String())
	}
}

func TestCLIWriteHelpHuman(t *testing.T) {
	var w cliWrites
	if err := WriteHelp(&w, cliGateSpec(), ModeHuman); err != nil {
		t.Fatal(err)
	}
	out := w.String()
	if w.n != 1 {
		t.Errorf("human help took %d writes", w.n)
	}
	for _, want := range []string{
		"measure this project's gates",
		"gate <gate> [flags]",
		"a gate this project answers",
		"`gate --list`",
		"-envelope, -no-envelope  (boolean)",
		"-list, -no-list  (boolean)",
		"print the gates this project provides",
		"-json, -human  (enumeration output-mode)",
		"-narrate  (boolean)",
		"-json-input <path>  (path)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("human help lacks %q:\n%s", want, out)
		}
	}
	// The root's own flags do not follow a command; the built-ins do.
	root, every := strings.Index(out, "flags without a command:"), strings.Index(out, "flags of every invocation:")
	list, json := strings.Index(out, "-list, -no-list"), strings.Index(out, "-json, -human")
	if root < 0 || every < 0 || !(root < list && list < every && every < json) {
		t.Errorf("-list is not under the root's flags and -json under every invocation's:\n%s", out)
	}
}

func TestCLIWriteVersion(t *testing.T) {
	var j, h bytes.Buffer
	if err := WriteVersion(&j, ModeJSON, "abc123"); err != nil {
		t.Fatal(err)
	}
	if err := WriteVersion(&h, ModeHuman, "abc123"); err != nil {
		t.Fatal(err)
	}
	if got, want := j.String(), `{"project":"promise","text":"abc123"}`+"\n"; got != want {
		t.Errorf("JSON version = %q, want %q", got, want)
	}
	if got, want := h.String(), "promise abc123\n"; got != want {
		t.Errorf("human version = %q, want %q", got, want)
	}

}

func TestCLIBareInvocation(t *testing.T) {
	gate := BareInvocation(cliGateSpec(), "abc123")
	if want := "expecting a subcommand: <gate>, a gate this project answers — `gate --list` names them\n" +
		"promise abc123\n" +
		"measure this project's gates\n" +
		"run `gate -help` for all of them"; gate.Error() != want {
		t.Errorf("gate's bare answer =\n%s\nwant\n%s", gate.Error(), want)
	}
	if ExitCode(gate) != 2 {
		t.Errorf("ExitCode = %d, want 2", ExitCode(gate))
	}
	run := BareInvocation(cliRunSpec(), "unknown")
	if want := "expecting a subcommand: <gate> | <command>, a gate to judge or a command to run — `run --list` names them\n" +
		"promise unknown\n" +
		"judge this project's gates\n" +
		"run `run -help` for all of them"; run.Error() != want {
		t.Errorf("run's bare answer =\n%s\nwant\n%s", run.Error(), want)
	}
}

// cliObserved restores whether the process is observed after the test.
func cliObserved(t *testing.T, observed bool) {
	t.Helper()
	saved := Observed()
	SetObserved(observed)
	t.Cleanup(func() { SetObserved(saved) })
}

func TestCLINarration(t *testing.T) {
	if !Observed() {
		t.Fatal("Observed() is false before anything set it; an unconverted tool would lose its narration")
	}
	stderr, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = stderr
	t.Cleanup(func() { os.Stderr = saved })

	if Narration() != stderr {
		t.Error("Narration() is not os.Stderr, as it stands now, when observed")
	}
	cliObserved(t, false)
	null := Narration()
	if null.Name() != os.DevNull {
		t.Errorf("unobserved Narration() = %s, want %s", null.Name(), os.DevNull)
	}
	if _, err := fmt.Fprintln(null, "nobody reads this"); err != nil {
		t.Errorf("writing to the null device: %v", err)
	}
	if Narration() != null {
		t.Error("the null device is opened again on every call")
	}
	SetObserved(true)
	if Narration() != stderr {
		t.Error("Narration() does not return to stderr once observed again")
	}
}

func TestCLISpecRefusesWhatCannotBeParsed(t *testing.T) {
	for _, spec := range []ToolSpec{
		{Tool: "x", Flags: []FlagSpec{{Name: "json"}}},
		{Tool: "x", Flags: []FlagSpec{{Name: "args"}}},
		{Tool: "x", Flags: []FlagSpec{{Name: "no-cache"}}},
		{Tool: "x", Flags: []FlagSpec{{Name: "dryRun"}}},
		{Tool: "x", Flags: []FlagSpec{{Name: "a"}, {Name: "a"}}},
		{Tool: "x", Flags: []FlagSpec{{Name: "out", Type: FlagPath}}},
		{Tool: "x", Commands: []CommandSpec{{Name: "<a>"}, {Name: "<b>"}}},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("ParseInvocation accepted the spec %+v", spec)
				}
			}()
			_, _ = ParseInvocation(spec, nil)
		}()
	}
}
