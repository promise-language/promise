package common

import (
	"path/filepath"
	"strings"
	"testing"
)

// T2116: tests for the guard that keeps toolchain discovery off the host's PATH.
// The fixtures below are Go sources staged into a throwaway repo; the lines that
// embed a lookup as fixture *text* carry the marker themselves, since this file
// is tracked Go source and the guard sweeps itself like anything else.

func TestCheckHostToolLookups_RejectsUnannotatedSite(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	stage("tools/build/common/x.go", []byte("package common\nfunc f() string { return Which(\"llvm-dlltool\") }\n")) // path-ok: fixture text for this guard's own test
	err := CheckHostToolLookups(root)
	if err == nil {
		t.Fatal("expected error for an unannotated host lookup, got nil")
	}
	if !strings.Contains(err.Error(), "tools/build/common/x.go:2") {
		t.Errorf("error should name the violating path AND line, got: %v", err)
	}
}

// The annotation is what turns a lookup into a reviewed decision, so a site
// carrying one with a reason is permitted wherever it lives.
func TestCheckHostToolLookups_AllowsAnnotatedSite(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	stage("compiler/cmd/promise/doctor.go",
		[]byte("package main\nfunc f() { exec.LookPath(\"wasmtime\") } // path-ok: doctor reports host state\n")) // path-ok: fixture text for this guard's own test
	if err := CheckHostToolLookups(root); err != nil {
		t.Fatalf("expected no error for an annotated site, got: %v", err)
	}
}

// A bare marker is not an exemption — the reason is the whole point, and this is
// the bug's own lesson: the rule existed, unwritten, and so was not applied.
func TestCheckHostToolLookups_RejectsMarkerWithoutReason(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	stage("tools/build/common/x.go",
		[]byte("package common\nfunc f() { Which(\"wasmtime\") } // path-ok:\n")) // path-ok: fixture text for this guard's own test
	if err := CheckHostToolLookups(root); err == nil {
		t.Fatal("expected error for a marker carrying no reason, got nil")
	}
}

// git, sh, go and gofmt are the environment the build runs inside, not tools it
// builds with, so looking one up needs no annotation.
func TestCheckHostToolLookups_AllowsEnvironmentTools(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	stage("tools/build/common/x.go",
		[]byte("package common\nfunc f() { exec.LookPath(\"git\"); Which(\"go\") }\n")) // path-ok: fixture text for this guard's own test
	if err := CheckHostToolLookups(root); err != nil {
		t.Fatalf("environment tools need no annotation, got: %v", err)
	}
}

// A computed argument cannot be judged from the source, so it is always a
// violation until someone says why — "it is only ever go" has to be written down.
func TestCheckHostToolLookups_RejectsComputedArgument(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	stage("tools/build/common/x.go", []byte("package common\nfunc f(n string) { Which(n) }\n")) // path-ok: fixture text for this guard's own test
	if err := CheckHostToolLookups(root); err == nil {
		t.Fatal("expected error for a lookup whose argument is not a literal, got nil")
	}
}

// exec.Command with a bare toolchain name resolves through PATH exactly as
// Which does. Without this the rule is bypassed by spelling it differently.
// Both halves of the name test are exercised: the `llvm-` family prefix, and a
// name listed outright.
func TestCheckHostToolLookups_RejectsBareToolchainCommand(t *testing.T) {
	for _, tool := range []string{"llvm-dlltool", "clang"} {
		t.Run(tool, func(t *testing.T) {
			root, stage := initGitRepoWithStager(t)
			stage("compiler/cmd/promise/x.go",
				[]byte("package main\nfunc f() { exec.Command(\""+tool+"\", \"-l\") }\n"))
			err := CheckHostToolLookups(root)
			if err == nil {
				t.Fatalf("expected error for a bare %s exec.Command, got nil", tool)
			}
			if !strings.Contains(err.Error(), "compiler/cmd/promise/x.go:2") {
				t.Errorf("error should name the site, got: %v", err)
			}
		})
	}
}

// An identifier that merely ends in the helper's name is not a call to it. The
// argument regex would match inside `fooWhich("opt")` on its own, so this pins
// that the word-boundary count is what decides — a false positive here would
// push authors to annotate lines that resolve nothing.
func TestCheckHostToolLookups_IgnoresIdentifierSuffix(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	stage("tools/build/common/x.go", []byte("package common\nfunc f() string { return fooWhich(\"opt\") }\n"))
	if err := CheckHostToolLookups(root); err != nil {
		t.Fatalf("fooWhich is not Which, got: %v", err)
	}
}

// The resolved-path form is the one we want everywhere, so it must not be
// flagged — nor must a non-toolchain program named bare.
func TestCheckHostToolLookups_AllowsResolvedPathAndNonToolchainCommands(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	stage("compiler/cmd/promise/x.go",
		[]byte("package main\nfunc f(tool string) { exec.Command(tool, \"-l\"); exec.Command(\"git\", \"status\") }\n"))
	if err := CheckHostToolLookups(root); err != nil {
		t.Fatalf("a resolved path and a non-toolchain program are both fine, got: %v", err)
	}
}

// A lookup named only in prose is not a lookup.
func TestCheckHostToolLookups_IgnoresComments(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	stage("tools/build/common/x.go",
		[]byte("package common\n// findLLVMTool never calls Which(\"opt\") — see T2108.\nfunc f() {}\n")) // path-ok: fixture text for this guard's own test
	if err := CheckHostToolLookups(root); err != nil {
		t.Fatalf("a mention in a comment is not a lookup, got: %v", err)
	}
}

// Declaring the helper is not using it — otherwise the definition every
// annotated caller goes through would report itself as the violation.
func TestCheckHostToolLookups_IgnoresDeclaration(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	stage("tools/build/common/x.go", []byte("package common\nfunc Which(name string) string { return name }\n")) // path-ok: fixture text for this guard's own test
	if err := CheckHostToolLookups(root); err != nil {
		t.Fatalf("a declaration is not a call, got: %v", err)
	}
}

// ANTLR writes the parser tree, so nothing in it is anyone's to annotate.
func TestCheckHostToolLookups_SkipsGeneratedParser(t *testing.T) {
	root, stage := initGitRepoWithStager(t)
	stage("compiler/internal/parser/promise_parser.go",
		[]byte("package parser\nfunc f() { Which(\"opt\") }\n")) // path-ok: fixture text for this guard's own test
	if err := CheckHostToolLookups(root); err != nil {
		t.Fatalf("generated sources are out of scope, got: %v", err)
	}
}

// A guard that cannot read the index must say so rather than report a clean
// tree: "no files to scan" and "no violations" are not the same answer. The root
// is a path that does not exist, so git fails the same way from any TMPDIR —
// one merely outside a repository would let git walk up and find the real one.
func TestCheckHostToolLookups_ErrorsWhenGitCannotList(t *testing.T) {
	err := CheckHostToolLookups(filepath.Join(t.TempDir(), "no-such-dir"))
	if err == nil {
		t.Fatal("expected an error when git cannot list the index, got nil")
	}
	if !strings.Contains(err.Error(), "list tracked *.go files") {
		t.Errorf("error should name the failing step, got: %v", err)
	}
}

// TestCheckHostToolLookups_ThisTreeIsClean runs the guard over the real
// checkout, so bin/verify fails on a violation and not only bin/precommit. The
// pre-commit hook is the other half; neither alone covers both the maintainer's
// commits and a plain verify run.
//
// It reads the index, so a brand-new file is covered from the moment it is
// `git add`ed and not before — which is also when the hook would see it.
func TestCheckHostToolLookups_ThisTreeIsClean(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if !Exists(filepath.Join(root, ".git")) {
		t.Skip("not a git checkout — the guard reads the index")
	}
	if err := CheckHostToolLookups(root); err != nil {
		t.Errorf("%v", err)
	}
}
