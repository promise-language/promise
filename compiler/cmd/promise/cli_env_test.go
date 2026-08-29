package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// cliEnv drives the built promise binary as a subprocess, with its own
// PROMISE_HOME and an isolated git configuration carried in each command's
// environment rather than the process's.
//
// Why not just call runAdd/runPkgUpdate in-process: those need the working
// directory and the environment to point at the fixture, and os.Chdir and
// t.Setenv are process-global. A test that uses them cannot run beside any
// other, which is what made the package-manager tests the serial floor of this
// package (T1776). Passing dir and env per command instead costs a fork and
// buys parallelism.
//
// It also removes the need for testVerifyCompilerBin: that hook exists because
// an in-process test's os.Executable() is the test binary rather than a
// compiler. A subprocess of the real binary is a real compiler, so the
// production code takes its normal path.
type cliEnv struct {
	bin  string
	home string
	env  []string
}

func newCLIEnv(t *testing.T) *cliEnv {
	t.Helper()
	bin := locatePromiseBin(t)
	home := t.TempDir()
	gitconfig := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(gitconfig,
		[]byte("[user]\n\temail = test@test.com\n\tname = Test\n[safe]\n\tdirectory = *\n"), 0644); err != nil {
		t.Fatalf("write git config: %v", err)
	}
	return &cliEnv{bin: bin, home: home, env: append(os.Environ(),
		"PROMISE_HOME="+home,
		"GIT_CONFIG_GLOBAL="+gitconfig,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
	)}
}

// run executes any command in dir under the isolated environment, failing the
// test if it does not succeed.
func (e *cliEnv) run(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = e.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v in %s: %v\n%s", name, args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// git runs a git command in dir, returning its trimmed output.
func (e *cliEnv) git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return e.run(t, dir, "git", args...)
}

// promise runs the compiler in dir and returns its combined output along with
// the exit error, for tests that assert on failure as well as success.
func (e *cliEnv) promise(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(e.bin, args...)
	cmd.Dir = dir
	cmd.Env = e.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// promiseOK runs the compiler in dir and fails the test if it does not succeed.
func (e *cliEnv) promiseOK(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := e.promise(t, dir, args...)
	if err != nil {
		t.Fatalf("promise %v in %s: %v\n%s", args, dir, err, out)
	}
	return out
}
