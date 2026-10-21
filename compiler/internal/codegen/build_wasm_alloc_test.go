package codegen

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// build_wasm_alloc.sh keeps wasm_alloc.o reproducible from the pinned LLVM
// release, and its refusals are the point: before it existed the object came
// from whichever clang was at hand (its producers section read "Homebrew
// clang"), and a guard that regressed to accepting any clang would fail nothing
// else — TestWasmAllocObjMatchesSource checks which symbols an object defines,
// not what built it.
//
// Each case runs a copy of the script in a scratch tree of the repository's
// shape, so the checked-in object is never touched, against a fake clang that
// reports the given version line and writes whatever -o names. The scratch tree
// is the working directory, not the script's own, so the object has to land
// beside the script regardless of where it is run from.
func TestBuildWasmAllocScriptBuildsOnlyWithThePinnedClang(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("build_wasm_alloc.sh is a bash script, run on a unix host")
	}
	script := filepath.Join(filepath.Dir(findWasmAllocSource(t)), "build_wasm_alloc.sh")

	// A section before [binaries.llvm] carries its own version, so taking the
	// first `version =` in the file instead of llvm's is caught.
	const prebuilts = `schema = 1

[binaries.wasmtime]
version = "9.9.2"

[binaries.llvm]
version = "9.9.1"

[binaries.llvm.targets.linux-amd64]
url = ""
`
	cases := []struct {
		name       string
		clang      string // version line the fake clang reports; "" passes no clang at all
		wantExit   int
		wantStderr string
	}{
		{name: "no clang given", wantExit: 2, wantStderr: "usage: build_wasm_alloc.sh <clang>"},
		{name: "vendor build of the pinned release", clang: "Homebrew clang version 9.9.1", wantExit: 1, wantStderr: "not the pinned LLVM 9.9.1"},
		{name: "release the pin is a prefix of", clang: "clang version 9.9.10 (https://github.com/llvm/llvm-project 0)", wantExit: 1, wantStderr: "not the pinned LLVM 9.9.1"},
		{name: "another section's version", clang: "clang version 9.9.2 (https://github.com/llvm/llvm-project 0)", wantExit: 1, wantStderr: "not the pinned LLVM 9.9.1"},
		{name: "pinned release", clang: "clang version 9.9.1 (https://github.com/llvm/llvm-project 0)", wantExit: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, crt := scratchWasmAllocTree(t, script, prebuilts)
			var args []string
			if tc.clang != "" {
				args = append(args, fakeClang(t, root, tc.clang))
			}
			exit, stdout, stderr := runBuildWasmAlloc(t, root, crt, args...)

			if exit != tc.wantExit {
				t.Fatalf("exit %d, want %d\nstderr:\n%s", exit, tc.wantExit, stderr)
			}
			if !strings.Contains(stderr, tc.wantStderr) {
				t.Errorf("stderr does not say %q:\n%s", tc.wantStderr, stderr)
			}
			obj, err := os.ReadFile(filepath.Join(crt, "wasm_alloc.o"))
			if err != nil {
				t.Fatal(err)
			}
			wantObj := "checked-in object\n"
			if tc.wantExit == 0 {
				wantObj = "rebuilt object\n"
				if stdout != "" {
					t.Errorf("stdout carries narration, which belongs on stderr:\n%s", stdout)
				}
			}
			if string(obj) != wantObj {
				t.Errorf("wasm_alloc.o holds %q, want %q — a refused clang must not get to write it", obj, wantObj)
			}
			if _, err := os.Stat(filepath.Join(root, "wasm_alloc.o")); err == nil {
				t.Error("the object was written to the working directory, not beside the script")
			}
		})
	}
}

// The cases above use a prebuilts.toml of the real one's shape; this reads the
// real one, so a restructuring that hides the LLVM pin from the script fails
// here rather than on the day someone has to rebuild the object.
func TestBuildWasmAllocScriptFindsTheRealLLVMPin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("build_wasm_alloc.sh is a bash script, run on a unix host")
	}
	script := filepath.Join(filepath.Dir(findWasmAllocSource(t)), "build_wasm_alloc.sh")
	realPrebuilts, err := os.ReadFile(filepath.Join(filepath.Dir(script), "..", "..", "..", "..", "..", "tools", "build", "prebuilts.toml"))
	if err != nil {
		t.Fatal(err)
	}

	root, crt := scratchWasmAllocTree(t, script, string(realPrebuilts))
	exit, _, stderr := runBuildWasmAlloc(t, root, crt, fakeClang(t, root, "clang version 0.0.0-unpinned (test)"))
	if exit != 1 || !strings.Contains(stderr, "not the pinned LLVM ") {
		t.Errorf("exit %d, want 1 refusing against the pinned version — the script found no [binaries.llvm] version in tools/build/prebuilts.toml\nstderr:\n%s", exit, stderr)
	}
}

// scratchWasmAllocTree lays out root/compiler/cmd/promise/crt/wasm32 with a copy
// of script, a source file and a stand-in checked-in object, and
// root/tools/build/prebuilts.toml with the given content.
func scratchWasmAllocTree(t *testing.T, script, prebuilts string) (root, crt string) {
	t.Helper()
	root = t.TempDir()
	crt = filepath.Join(root, "compiler", "cmd", "promise", "crt", "wasm32")
	tools := filepath.Join(root, "tools", "build")
	for _, dir := range []string{crt, tools} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	files := []struct {
		path string
		body string
		mode os.FileMode
	}{
		{filepath.Join(crt, "build_wasm_alloc.sh"), string(body), 0o755},
		{filepath.Join(crt, "wasm_alloc.c"), "int cabi_example(void) { return 0; }\n", 0o644},
		{filepath.Join(crt, "wasm_alloc.o"), "checked-in object\n", 0o644},
		{filepath.Join(tools, "prebuilts.toml"), prebuilts, 0o644},
	}
	for _, f := range files {
		if err := os.WriteFile(f.path, []byte(f.body), f.mode); err != nil {
			t.Fatal(err)
		}
	}
	return root, crt
}

// fakeClang writes a clang stand-in under root that prints version for
// --version and otherwise writes "rebuilt object" to the file -o names.
func fakeClang(t *testing.T, root, version string) string {
	t.Helper()
	path := filepath.Join(root, "fake-clang")
	body := "#!/bin/sh\n" +
		"if [ \"$1\" = --version ]; then\n" +
		"  printf '%s\\n' '" + version + "' 'Target: wasm32-unknown-wasip1'\n" +
		"  exit 0\n" +
		"fi\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  if [ \"$1\" = -o ]; then printf 'rebuilt object\\n' > \"$2\"; fi\n" +
		"  shift\n" +
		"done\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// runBuildWasmAlloc runs the script copy in crt from root, returning its exit
// status and output.
func runBuildWasmAlloc(t *testing.T, root, crt string, args ...string) (exit int, stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(filepath.Join(crt, "build_wasm_alloc.sh"), args...)
	cmd.Dir = root
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		exit = exitErr.ExitCode()
	default:
		t.Fatalf("run build_wasm_alloc.sh: %v", err)
	}
	return exit, out.String(), errOut.String()
}
