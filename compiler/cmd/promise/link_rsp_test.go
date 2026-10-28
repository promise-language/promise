package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// #619: lld-link and wasm-ld take their arguments through a response file. The
// inline command line grows with every object times its path, and Windows'
// CreateProcess refuses one past windowsCommandLineMax characters — which is
// how tests/std/map_test.pr stopped linking on windows-amd64.

// windowsCommandLineMax is CreateProcess's lpCommandLine limit, in characters.
const windowsCommandLineMax = 32767

func TestWindowsQuoteArg(t *testing.T) {
	t.Parallel()
	// Expected values follow CommandLineToArgvW, which LLVM's
	// TokenizeWindowsCommandLine implements: backslashes are literal unless a
	// run of them ends at a double quote.
	for _, tc := range []struct {
		name, in, want string
	}{
		{"plain path", `C:\a\b.obj`, `C:\a\b.obj`},
		{"flag", `/out:prog.exe`, `/out:prog.exe`},
		{"non-ASCII", `C:\Users\Zoë\x.o`, `C:\Users\Zoë\x.o`},
		{"space", `/libpath:C:\Program Files\x`, `"/libpath:C:\Program Files\x"`},
		{"empty", ``, `""`},
		{"quote", `a"b`, `"a\"b"`},
		{"backslash before quote", `a\"b`, `"a\\\"b"`},
		// Two backslashes, so doubling the run (2n+1, 2n) is told apart from
		// adding one to it (n+2, n+1), which a single backslash cannot do.
		{"backslash run before quote", `a\\"b`, `"a\\\\\"b"`},
		{"trailing backslash", `C:\dir with space\`, `"C:\dir with space\\"`},
		{"trailing backslash run", `C:\dir with space\\`, `"C:\dir with space\\\\"`},
		{"tab", "a\tb", "\"a\tb\""},
		{"newline", "a\nb", "\"a\nb\""},
		{"carriage return", "a\rb", "\"a\rb\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := windowsQuoteArg(tc.in); got != tc.want {
				t.Errorf("windowsQuoteArg(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestWriteResponseFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	out := filepath.Join(dir, "prog.exe")
	rsp, err := writeResponseFile(out, []string{
		"/nologo",
		`/out:C:\dir with space\a.exe`,
		`C:\a\b.obj`,
	})
	if err != nil {
		t.Fatalf("writeResponseFile: %v", err)
	}
	defer os.Remove(rsp)

	if filepath.Dir(rsp) != dir {
		t.Errorf("response file %s is not beside the output in %s", rsp, dir)
	}
	if !strings.HasSuffix(rsp, ".rsp") {
		t.Errorf("response file %s does not end in .rsp", rsp)
	}
	got, err := os.ReadFile(rsp)
	if err != nil {
		t.Fatal(err)
	}
	want := "/nologo\n" +
		`"/out:C:\dir with space\a.exe"` + "\n" +
		`C:\a\b.obj` + "\n"
	if string(got) != want {
		t.Errorf("response file content = %q, want %q", got, want)
	}
}

func TestWriteResponseFileMissingDir(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "missing")
	rsp, err := writeResponseFile(filepath.Join(missing, "prog.exe"), []string{"/nologo"})
	if err == nil {
		os.Remove(rsp)
		t.Fatalf("writeResponseFile into a missing directory returned %s, want an error", rsp)
	}
	if !strings.Contains(err.Error(), "response file") {
		t.Errorf("error %q does not name the response file", err)
	}
	if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
		t.Errorf("writeResponseFile created %s (stat err %v)", missing, statErr)
	}
}

// rspLinker is one linker that takes its arguments through a response file.
type rspLinker struct {
	name     string
	tool     string
	target   string
	entryIR  string // defines the symbol the link needs as its entry/export
	magic    string // the output's leading bytes
	outName  string
	linkArgs func(objFiles []string, outputFile string) ([]string, error)
	link     func(objFiles []string, outputFile string) error
}

// rspLinkers returns the linkers that take a response file, one per case of the
// link tests below.
func rspLinkers() []rspLinker {
	return []rspLinker{{
		name:    "lld-link",
		tool:    "lld-link",
		target:  windowsAmd64Triple,
		entryIR: "define void @__promise_start() {\nentry:\n  ret void\n}\n",
		magic:   "MZ",
		outName: "prog.exe",
		linkArgs: func(objFiles []string, outputFile string) ([]string, error) {
			return buildWindowsLinkArgs(windowsAmd64Triple, objFiles, outputFile)
		},
		link: func(objFiles []string, outputFile string) error {
			return linkWindowsMulti(objFiles, windowsAmd64Triple, outputFile)
		},
	}, {
		name:    "wasm-ld",
		tool:    "wasm-ld",
		target:  "wasm32-wasi",
		entryIR: "define void @_start() {\nentry:\n  ret void\n}\n",
		magic:   "\x00asm",
		outName: "prog.wasm",
		linkArgs: func(objFiles []string, outputFile string) ([]string, error) {
			return buildWasmLinkArgs(objFiles, "wasm32-wasi", outputFile, false)
		},
		link: func(objFiles []string, outputFile string) error {
			return linkWasmMulti(objFiles, "wasm32-wasi", outputFile, false)
		},
	}}
}

// paddingObjects compiles an empty module for target once and copies it to n
// distinct files under a long directory whose name carries a space and a
// non-ASCII letter — the shapes a real %TEMP% (a profile path, a user name)
// puts in front of every object, and the ones the response file must quote and
// encode. Each path stays well under 260 characters, so nothing here depends on
// Windows long-path support; the length comes from the count.
//
// Off Windows the name also carries a backslash, which every Windows path
// already holds as its separator: Windows quoting keeps it literal, and GNU
// quoting — wasm-ld's default for a response file on a non-Windows host —
// drops it, so the link fails there unless --rsp-quoting=windows is passed.
func paddingObjects(t *testing.T, optPath, llcPath, target string, n int) []string {
	t.Helper()
	obj, err := compileLLToObj("; padding\n", "rsppad", target, optPath, llcPath, "-O1")
	if err != nil {
		t.Fatalf("compiling the padding object: %v", err)
	}
	data, err := os.ReadFile(obj)
	os.Remove(obj)
	if err != nil {
		t.Fatal(err)
	}
	name := "rsp padding é " + strings.Repeat("d", 76)
	if runtime.GOOS != "windows" {
		name += `\b`
	}
	dir := filepath.Join(t.TempDir(), name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	paths := make([]string, n)
	for i := range paths {
		paths[i] = filepath.Join(dir, fmt.Sprintf("pad-%04d.o", i))
		if err := os.WriteFile(paths[i], data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return paths
}

// requireOnly fails the test if dir holds anything but the names in want.
func requireOnly(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s holds %q, want %q — a response file left behind is a leaked temp", dir, got, want)
	}
}

// commandLineLen is the length of args joined into one command line.
func commandLineLen(args []string) int {
	n := 0
	for _, a := range args {
		n += len(a) + 1
	}
	return n
}

// requireOverrun fails the test unless linking objFiles into out would, with
// the arguments inline, overrun CreateProcess's limit — without that, a link
// test passes whether or not the response file is used. It returns the inline
// length.
func requireOverrun(t *testing.T, l rspLinker, objFiles []string, out string) int {
	t.Helper()
	args, err := l.linkArgs(objFiles, out)
	if err != nil {
		t.Fatal(err)
	}
	inline := commandLineLen(args)
	if inline <= windowsCommandLineMax {
		t.Fatalf("inline command line is %d characters, want more than %d", inline, windowsCommandLineMax)
	}
	return inline
}

// TestLinkThroughResponseFile is #619's regression test: a link whose inline
// command line would exceed CreateProcess's limit links. On Windows, passing
// the objects on argv again fails here with "The filename or extension is too
// long"; on every host, the real linker reads the quoted, non-ASCII,
// backslashed paths back out of the response file.
func TestLinkThroughResponseFile(t *testing.T) {
	optPath, llcPath := requireLLVMTools(t)
	for _, l := range rspLinkers() {
		t.Run(l.name, func(t *testing.T) {
			if _, err := findLLVMTool(l.tool); err != nil {
				t.Skipf("%s not available: %v", l.tool, err)
			}
			entry, err := compileLLToObj(l.entryIR, "rspentry", l.target, optPath, llcPath, "-O1")
			if err != nil {
				t.Fatalf("compiling the entry object: %v", err)
			}
			defer os.Remove(entry)
			objFiles := append([]string{entry}, paddingObjects(t, optPath, llcPath, l.target, 500)...)

			outDir := filepath.Join(t.TempDir(), "out")
			if err := os.Mkdir(outDir, 0o755); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(outDir, l.outName)

			inline := requireOverrun(t, l, objFiles, out)
			if err := l.link(objFiles, out); err != nil {
				t.Fatalf("%s link of %d objects (%d-character inline command line): %v", l.tool, len(objFiles), inline, err)
			}
			got, err := os.ReadFile(out)
			if err != nil {
				t.Fatalf("link reported success but wrote no %s: %v", out, err)
			}
			if !bytes.HasPrefix(got, []byte(l.magic)) {
				t.Errorf("%s does not start with %q", out, l.magic)
			}
			requireOnly(t, outDir, l.outName)
		})
	}
}

// TestLinkThroughResponseFileFailure: a link the linker rejects — here, an
// object that does not exist — returns the linker's error and still removes
// the response file. (A missing entry symbol is no failure for wasm-ld, which
// links with --allow-undefined.)
func TestLinkThroughResponseFileFailure(t *testing.T) {
	for _, l := range rspLinkers() {
		t.Run(l.name, func(t *testing.T) {
			if _, err := findLLVMTool(l.tool); err != nil {
				t.Skipf("%s not available: %v", l.tool, err)
			}
			missing := filepath.Join(t.TempDir(), "missing.o")
			outDir := t.TempDir()

			var err error
			withSilencedStderr(t, func() {
				err = l.link([]string{missing}, filepath.Join(outDir, l.outName))
			})
			if err == nil {
				t.Fatalf("%s link of a missing object succeeded, want an error", l.tool)
			}
			if want := "error linking (" + l.tool + ")"; !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not say %q", err, want)
			}
			requireOnly(t, outDir)
		})
	}
}

// fakeLinkerEnv names the file a fake linker records its invocation in. While
// it is set, TestMain runs this test binary as that linker (fakeLinkerMain)
// instead of running tests, so a test can point PROMISE_LLD / PROMISE_WASM_LD
// at os.Args[0] and read back what the compiler spawned.
const fakeLinkerEnv = "PROMISE_TEST_FAKE_LINKER"

// fakeLinkerCall is one recorded linker invocation: its argv, and the contents
// of each @file argument as the linker found it while it ran.
type fakeLinkerCall struct {
	Args      []string
	Responses map[string]string
}

// fakeLinkerMain is the fake linker. It answers checkLLVMToolVersion's
// --version probe with no version, so the check passes it by, and records every
// other invocation in record, overwriting any earlier one.
func fakeLinkerMain(record string) int {
	args := os.Args[1:]
	if len(args) == 1 && args[0] == "--version" {
		return 0
	}
	call := fakeLinkerCall{Args: args, Responses: map[string]string{}}
	for _, a := range args {
		if name, ok := strings.CutPrefix(a, "@"); ok {
			data, err := os.ReadFile(name)
			if err != nil {
				fmt.Fprintln(os.Stderr, "fake linker:", err)
				return 1
			}
			call.Responses[name] = string(data)
		}
	}
	data, err := json.Marshal(call)
	if err == nil {
		err = os.WriteFile(record, data, 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake linker:", err)
		return 1
	}
	return 0
}

// TestLinkerArgvIsResponseFile is #619's regression test on every host, where
// TestLinkThroughResponseFile catches a revert only on Windows: the argv each
// linker is spawned with stays under CreateProcess's limit however many objects
// there are, and the response file it names — beside the output, and gone once
// the link returns — carries every object. A fake linker records the spawn, so
// no LLVM toolchain is needed.
func TestLinkerArgvIsResponseFile(t *testing.T) {
	for _, l := range rspLinkers() {
		t.Run(l.name, func(t *testing.T) {
			clearToolchainOverrides(t)
			oldWarn := toolchainWarnW
			toolchainWarnW = io.Discard
			t.Cleanup(func() { toolchainWarnW = oldWarn })
			record := filepath.Join(t.TempDir(), "record.json")
			t.Setenv(fakeLinkerEnv, record)
			t.Setenv(llvmToolEnvVars[l.tool], os.Args[0])

			// The fake linker opens none of the objects, so they need not exist.
			dir := filepath.Join(t.TempDir(), strings.Repeat("d", 100))
			objFiles := make([]string, 500)
			for i := range objFiles {
				objFiles[i] = filepath.Join(dir, fmt.Sprintf("obj-%04d.o", i))
			}
			outDir := filepath.Join(t.TempDir(), "out")
			if err := os.Mkdir(outDir, 0o755); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(outDir, l.outName)
			requireOverrun(t, l, objFiles, out)

			if err := l.link(objFiles, out); err != nil {
				t.Fatalf("%s link: %v", l.tool, err)
			}
			data, err := os.ReadFile(record)
			if err != nil {
				t.Fatalf("the fake %s never ran: %v", l.tool, err)
			}
			var call fakeLinkerCall
			if err := json.Unmarshal(data, &call); err != nil {
				t.Fatal(err)
			}

			head := call.Args[:min(len(call.Args), 4)] // the argv being judged may be 100K long
			if n := len(os.Args[0]) + 1 + commandLineLen(call.Args); n > windowsCommandLineMax {
				t.Errorf("%s was spawned with a %d-character command line, over the %d limit, starting %q", l.tool, n, windowsCommandLineMax, head)
			}
			if len(call.Responses) != 1 {
				t.Fatalf("%s got %d response files, want 1, in %d arguments starting %q", l.tool, len(call.Responses), len(call.Args), head)
			}
			for rsp, content := range call.Responses {
				if filepath.Dir(rsp) != outDir {
					t.Errorf("response file %s is not beside the output in %s", rsp, outDir)
				}
				lines := map[string]bool{}
				for _, line := range strings.Split(content, "\n") {
					lines[line] = true
				}
				for _, o := range objFiles {
					if !lines[windowsQuoteArg(o)] {
						t.Fatalf("response file has no line for %s:\n%s", o, content)
					}
				}
			}
			requireOnly(t, outDir)
		})
	}
}
