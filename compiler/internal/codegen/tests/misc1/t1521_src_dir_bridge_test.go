package misc1

// T1521 — the os.src_dir bridge, at IR level.
//
// promise_os_get_src_dir is the one os bridge that calls no PAL function: the
// answer is a compile-time constant carried in sema.Info.SourceDir, so the whole
// body is decided by codegen. The two branches are the item's central promise —
// a program with a source directory gets exactly that directory, and one without
// (`promise exec`, whose inline program lives nowhere) gets `none` rather than a
// fabricated path. The second branch is invisible to the Promise-level tests,
// which can only ever compile from a file and therefore only ever see the first.

import (
	"strconv"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen"
	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// srcDirBridgeSrc declares the extern that modules/os/os.pr declares, which is
// what puts promise_os_get_src_dir in the IR for defineOSBodies to fill in.
const srcDirBridgeSrc = `_os_src_dir() string? ` + "`extern" + `("promise_os_get_src_dir");

main() {
  if d := _os_src_dir() {
  }
}
`

func srcDirBridgeIR(t *testing.T, sourceDir string) string {
	t.Helper()
	file, info := codegentest.ParseWithStd(t, srcDirBridgeSrc)
	info.SourceDir = sourceDir
	return codegen.Compile(file, info, "").Module.String()
}

// srcDirBridgeBody returns the bridge's own definition. FuncBody is no help here:
// it looks for a @__user.-prefixed Promise function, and this is a runtime symbol.
func srcDirBridgeBody(t *testing.T, ir string) string {
	t.Helper()
	body := codegentest.ExtractDefine(ir, "promise_os_get_src_dir")
	if body == "" {
		t.Fatalf("no definition of @promise_os_get_src_dir in IR:\n%s", ir)
	}
	return body
}

func TestSrcDirBridgeBakesTheSourceDirectory(t *testing.T) {
	const dir = "/home/agent/tools/make"
	ir := srcDirBridgeIR(t, dir)
	body := srcDirBridgeBody(t, ir)

	// The path is copied out of .rodata into a fresh heap string, exactly as the
	// PAL-backed string bridges do, so the caller's drop path is the usual one.
	if got := codegentest.StringNewCount(body); got != 1 {
		t.Errorf("promise_string_new call count = %d, want 1, in body:\n%s", got, body)
	}
	// The directory itself has to reach the binary as a constant.
	if !strings.Contains(ir, dir) {
		t.Errorf("IR does not carry the source directory %q", dir)
	}
	// The length handed to promise_string_new is the path's, excluding the NUL
	// terminator the .rodata constant carries — one byte too many would append a
	// stray NUL to every path a tool builds from it.
	if want := "i64 " + strconv.Itoa(len(dir)) + ")"; !strings.Contains(body, want) {
		t.Errorf("body does not pass the path length %q:\n%s", want, body)
	}
	// Present, not none.
	if !strings.Contains(body, "i1 true, 0") {
		t.Errorf("body does not build a present optional:\n%s", body)
	}
}

// An empty SourceDir is how `promise exec` reaches codegen: the program has no
// source directory, so the bridge must store `none` and must not invent a path.
// A regression here would not fail to compile — it would hand every inline
// program some plausible-looking directory, which is the exact failure (a path
// into the build cache, as with go run's os.Args[0]) the accessor exists to avoid.
func TestSrcDirBridgeWithoutSourceDirReturnsNone(t *testing.T) {
	ir := srcDirBridgeIR(t, "")
	body := srcDirBridgeBody(t, ir)

	if got := codegentest.StringNewCount(body); got != 0 {
		t.Errorf("promise_string_new call count = %d, want 0 (there is no path to allocate), in body:\n%s", got, body)
	}
	if !strings.Contains(body, "i1 false, 0") || !strings.Contains(body, "i8* null, 1") {
		t.Errorf("body does not build a none optional:\n%s", body)
	}
	// Nothing may be read out of .rodata here: no constant is the right one.
	if strings.Contains(body, "@.cstr") || strings.Contains(body, "@.str") {
		t.Errorf("body references a string constant while having no source directory:\n%s", body)
	}
}

// A relative SourceDir can never reach codegen — cmd/promise's setProgramSourceDir
// only records absolute paths — but codegen bakes whatever it is handed verbatim,
// so pin that: no cleaning, no cwd-joining, no silent substitution of a different
// directory. What the frontend decided is what the program observes.
func TestSrcDirBridgeBakesTheValueVerbatim(t *testing.T) {
	const dir = "/tmp/a b/c-d.e/tools"
	ir := srcDirBridgeIR(t, dir)
	body := srcDirBridgeBody(t, ir)

	if !strings.Contains(ir, dir) {
		t.Errorf("IR does not carry %q verbatim", dir)
	}
	if want := "i64 " + strconv.Itoa(len(dir)) + ")"; !strings.Contains(body, want) {
		t.Errorf("body does not pass the exact path length %q:\n%s", want, body)
	}
}
