package module

import (
	"strings"
	"testing"
)

func TestSanitizeIRPrefixSimpleIdent(t *testing.T) {
	// Simple identifiers pass through unchanged
	tests := []struct{ input, want string }{
		{"mylib", "mylib"},
		{"json", "json"},
		{"http", "http"},
		{"my_lib", "my_lib"},
		{"_private", "_private"},
		{"Lib123", "Lib123"},
	}
	for _, tc := range tests {
		got := SanitizeIRPrefix(tc.input)
		if got != tc.want {
			t.Errorf("SanitizeIRPrefix(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestSanitizeIRPrefixLocalPaths(t *testing.T) {
	// Single-component local paths get hash suffix to avoid collisions with catalog names.
	// "./mylib" must NOT equal "mylib" (catalog vs local disambiguation).
	tests := []string{"./mylib", "./counter", "./helpers"}
	for _, input := range tests {
		got := SanitizeIRPrefix(input)
		base := stripPathPrefixes(input) // e.g., "mylib"
		if !strings.HasPrefix(got, base+"_") {
			t.Errorf("SanitizeIRPrefix(%q) = %q, expected prefix %q", input, got, base+"_")
		}
		// Must differ from the catalog version (no ./ prefix)
		catalog := SanitizeIRPrefix(base)
		if got == catalog {
			t.Errorf("collision: SanitizeIRPrefix(%q) == SanitizeIRPrefix(%q) == %q", input, base, got)
		}
	}
}

func TestSanitizeIRPrefixMultiComponentPaths(t *testing.T) {
	// Multi-component paths get sanitized with hash suffix
	p1 := SanitizeIRPrefix("./libs/parser")
	if p1 == "" {
		t.Fatal("expected non-empty prefix")
	}
	// Should contain the sanitized components
	if !strings.Contains(p1, "libs_parser") {
		t.Errorf("expected %q to contain 'libs_parser'", p1)
	}
	// Should have a hash suffix (6 hex chars after last _)
	if len(p1) <= len("libs_parser_") {
		t.Errorf("expected hash suffix in %q", p1)
	}
}

func TestSanitizeIRPrefixRemoteURLs(t *testing.T) {
	// Remote URLs get sanitized with hash suffix
	p := SanitizeIRPrefix("github.com/alice/parser")
	if p == "" {
		t.Fatal("expected non-empty prefix")
	}
	if !strings.Contains(p, "github_com_alice_parser") {
		t.Errorf("expected %q to contain 'github_com_alice_parser'", p)
	}
}

func TestSanitizeIRPrefixCollisionFreedom(t *testing.T) {
	// Two different global identities must produce different IR prefixes
	tests := []struct{ a, b string }{
		{"github.com/alice/parser", "github.com/bob/parser"},
		{"./libs/parser", "github.com/alice/parser"},
		{"./libs_parser", "./libs/parser"},
		{"github.com/alice_parser", "github.com/alice/parser"},
		// Critical: catalog vs local module with same name must not collide
		{"json", "./json"},
		{"mylib", "./mylib"},
		{"parser", "./parser"},
		{"http", "../http"},
	}
	for _, tc := range tests {
		pa := SanitizeIRPrefix(tc.a)
		pb := SanitizeIRPrefix(tc.b)
		if pa == pb {
			t.Errorf("collision: SanitizeIRPrefix(%q) == SanitizeIRPrefix(%q) == %q", tc.a, tc.b, pa)
		}
	}
}

func TestSanitizeIRPrefixStability(t *testing.T) {
	// Same input always produces same output
	inputs := []string{
		"mylib",
		"./libs/parser",
		"github.com/alice/parser",
		"github.com/bob/parser",
	}
	for _, input := range inputs {
		a := SanitizeIRPrefix(input)
		b := SanitizeIRPrefix(input)
		if a != b {
			t.Errorf("unstable: SanitizeIRPrefix(%q) returned %q then %q", input, a, b)
		}
	}
}

func TestSanitizeIRPrefixStartsWithLetter(t *testing.T) {
	// All outputs must start with a letter (valid C/LLVM identifier)
	inputs := []string{
		"github.com/alice/parser",
		"./libs/parser",
		"123numeric",
		"---special---",
		"",
	}
	for _, input := range inputs {
		got := SanitizeIRPrefix(input)
		if len(got) == 0 {
			t.Errorf("SanitizeIRPrefix(%q) returned empty string", input)
			continue
		}
		if !isLetter(rune(got[0])) && got[0] != '_' {
			t.Errorf("SanitizeIRPrefix(%q) = %q, starts with non-letter %q", input, got, string(got[0]))
		}
	}
}

func TestSanitizeIRPrefixMultipleParentDirs(t *testing.T) {
	// Multiple ../ components should all be stripped
	p := SanitizeIRPrefix("../../deep/path")
	if strings.Contains(p, "..") {
		t.Errorf("expected no '..' in %q", p)
	}
	if !strings.Contains(p, "deep_path") {
		t.Errorf("expected %q to contain 'deep_path'", p)
	}
}

func TestGlobalIdentityFunctions(t *testing.T) {
	if got := GlobalIdentityForLocal("./mylib"); got != "./mylib" {
		t.Errorf("GlobalIdentityForLocal = %q", got)
	}
	if got := GlobalIdentityForRemote("github.com/alice/parser"); got != "github.com/alice/parser" {
		t.Errorf("GlobalIdentityForRemote = %q", got)
	}
	if got := GlobalIdentityForCatalog("json"); got != "json" {
		t.Errorf("GlobalIdentityForCatalog = %q", got)
	}
}

func TestStripPathPrefixes(t *testing.T) {
	tests := []struct{ input, want string }{
		{"./mylib", "mylib"},
		{"../mylib", "mylib"},
		{"../../deep/path", "deep/path"},
		{"./././foo", "foo"},
		{"../../../a", "a"},
		{"foo", "foo"},
		{"", ""},
	}
	for _, tc := range tests {
		got := stripPathPrefixes(tc.input)
		if got != tc.want {
			t.Errorf("stripPathPrefixes(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestEnsureLetterStart(t *testing.T) {
	tests := []struct{ input, want string }{
		{"abc", "abc"},
		{"123", "m123"},
		{"", "m"},
		{"_foo", "_foo"},
	}
	for _, tc := range tests {
		got := ensureLetterStart(tc.input)
		if got != tc.want {
			t.Errorf("ensureLetterStart(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
