package main

import (
	"strings"
	"testing"
)

// TestParseCLIArgs exercises the shared strict parser directly (T1604).
func TestParseCLIArgs(t *testing.T) {
	t.Run("value and bool flags with a single positional", func(t *testing.T) {
		var target string
		var release bool
		res, err := parseCLIArgs("build", []string{"-target", "wasm32-wasi", "-release", "main.pr"},
			flagSpec{
				value: map[string]*string{"target": &target},
				flag:  map[string]*bool{"release": &release},
			}, false, false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if target != "wasm32-wasi" {
			t.Errorf("target = %q, want wasm32-wasi", target)
		}
		if !release {
			t.Errorf("release not set")
		}
		if len(res.positionals) != 1 || res.positionals[0] != "main.pr" {
			t.Errorf("positionals = %#v, want [main.pr]", res.positionals)
		}
	})

	t.Run("unknown flag errors without prog-arg hint", func(t *testing.T) {
		_, err := parseCLIArgs("build", []string{"-relase", "main.pr"}, flagSpec{}, false, false)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "unknown flag -relase") {
			t.Errorf("error missing unknown-flag text: %v", err)
		}
		if strings.Contains(err.Error(), "after --") {
			t.Errorf("build (no prog args) must not suggest --: %v", err)
		}
	})

	t.Run("unknown flag on run points at --", func(t *testing.T) {
		_, err := parseCLIArgs("run", []string{"-relase", "main.pr"}, flagSpec{}, true, false)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "promise run <file.pr> -- -relase") {
			t.Errorf("run hint missing -- escape: %v", err)
		}
	})

	t.Run("value flag missing its value", func(t *testing.T) {
		var target string
		_, err := parseCLIArgs("build", []string{"main.pr", "-target"},
			flagSpec{value: map[string]*string{"target": &target}}, false, false)
		if err == nil || !strings.Contains(err.Error(), "requires a value") {
			t.Fatalf("expected requires-a-value error, got %v", err)
		}
	})

	t.Run("second positional rejected when not allowed", func(t *testing.T) {
		_, err := parseCLIArgs("build", []string{"a.pr", "b.pr"}, flagSpec{}, false, false)
		if err == nil || !strings.Contains(err.Error(), "unexpected extra argument") {
			t.Fatalf("expected extra-argument error, got %v", err)
		}
	})

	t.Run("many positionals allowed for exec", func(t *testing.T) {
		res, err := parseCLIArgs("exec", []string{"print_line(", "x", ")"}, flagSpec{}, false, true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(res.positionals) != 3 {
			t.Errorf("positionals = %#v, want 3", res.positionals)
		}
	})

	t.Run("bare -- rejected when prog args unsupported", func(t *testing.T) {
		_, err := parseCLIArgs("build", []string{"main.pr", "--", "junk"}, flagSpec{}, false, false)
		if err == nil || !strings.Contains(err.Error(), "does not forward program arguments") {
			t.Fatalf("expected `--` rejection, got %v", err)
		}
	})

	t.Run("bare -- splits program argv when supported", func(t *testing.T) {
		res, err := parseCLIArgs("run", []string{"app.pr", "--", "--verbose", "x"}, flagSpec{}, true, false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(res.positionals) != 1 || res.positionals[0] != "app.pr" {
			t.Errorf("positionals = %#v, want [app.pr]", res.positionals)
		}
		if len(res.progArgs) != 2 || res.progArgs[0] != "--verbose" || res.progArgs[1] != "x" {
			t.Errorf("progArgs = %#v, want [--verbose x]", res.progArgs)
		}
	})

	t.Run("lone dash is a positional, not a flag", func(t *testing.T) {
		res, err := parseCLIArgs("exec", []string{"-"}, flagSpec{}, false, true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(res.positionals) != 1 || res.positionals[0] != "-" {
			t.Errorf("positionals = %#v, want [-]", res.positionals)
		}
	})
}
