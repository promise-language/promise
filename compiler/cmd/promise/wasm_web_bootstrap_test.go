package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
)

// TestEmitWebBootstrapJSIncludesBasePALGlue verifies the auto-generated
// wasm32-web bootstrap loader implements the base PAL runtime imports
// (promise_env.write/exit/monotonic_nanos) that every wasm32-web binary
// unconditionally imports — WebIDL bindings or not. Before this fix, the
// bootstrap only did WebAssembly.instantiateStreaming with an empty default
// importObject, leaving these unresolved for any program not paired with
// hand-written or WebIDL-bindgen-generated glue.
func TestEmitWebBootstrapJSIncludesBasePALGlue(t *testing.T) {
	dir := t.TempDir()
	wasmFile := filepath.Join(dir, "frontend.wasm")

	emitWebBootstrapJS(wasmFile)

	jsFile := filepath.Join(dir, "frontend.js")
	data, err := os.ReadFile(jsFile)
	if err != nil {
		t.Fatalf("expected bootstrap JS at %s: %v", jsFile, err)
	}
	content := string(data)

	for _, want := range []string{"write(", "exit(", "monotonic_nanos("} {
		if !strings.Contains(content, want) {
			t.Errorf("bootstrap JS missing base PAL import %q", want)
		}
	}
}

// TestEmitWebBootstrapJSMergesImportObject verifies init() adds a
// caller-supplied importObject.promise_env to the base PAL defaults instead of
// replacing them outright — so a page can supply a function a hand-declared
// `wasm_import` names (examples/11_wasm/web_console.pr) and still get the base
// imports. TestBuildWebBootstrapLoadsProgram is the behavioral proof.
func TestEmitWebBootstrapJSMergesImportObject(t *testing.T) {
	dir := t.TempDir()
	wasmFile := filepath.Join(dir, "frontend.wasm")

	emitWebBootstrapJS(wasmFile)

	data, err := os.ReadFile(filepath.Join(dir, "frontend.js"))
	if err != nil {
		t.Fatalf("read bootstrap JS: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "...basePromiseEnv") || !strings.Contains(content, "...(importObject.promise_env || {})") {
		t.Error("init() should merge importObject.promise_env on top of basePromiseEnv, not replace it")
	}
	if !strings.Contains(content, "...importObject") {
		t.Error("init() should spread the caller's importObject rather than only accepting {}")
	}
}

// TestEmitWebBootstrapJSCatchesExitUnwind verifies that init() catches the
// exit-unwind sentinel around _initialize(): once base PAL glue provides a
// real (throwing) exit() implementation, a normal successful main() return —
// which lowers to pal_exit(0) — must not surface as an uncaught JS exception.
// It also checks for the typed _ExitSignal sentinel (rather than matching a
// bare Error's .message string, which would swallow any unrelated JS error
// carrying the same text) and that a non-zero code is checked, not just the
// sentinel's identity — see internal/bindgen's
// TestLoaderSmokeSurfacesNonZeroExit for the corresponding behavioral proof
// that exit(1) actually propagates.
func TestEmitWebBootstrapJSCatchesExitUnwind(t *testing.T) {
	dir := t.TempDir()
	wasmFile := filepath.Join(dir, "frontend.wasm")

	emitWebBootstrapJS(wasmFile)

	data, err := os.ReadFile(filepath.Join(dir, "frontend.js"))
	if err != nil {
		t.Fatalf("read bootstrap JS: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "class _ExitSignal") {
		t.Error("expected a typed _ExitSignal sentinel class instead of bare Error message matching")
	}
	if !strings.Contains(content, "try {") || !strings.Contains(content, "instanceof _ExitSignal") {
		t.Error("init() should try/catch _initialize() and check instanceof _ExitSignal")
	}
	if !strings.Contains(content, "e.code !== 0") {
		t.Error("init() should distinguish exit(0) from a non-zero exit code, not swallow every exit unconditionally")
	}
}

// bootstrapLoadPrograms are real programs for TestBuildWebBootstrapLoadsProgram.
// host declares one import of its own, which no generated JS supplies — the
// page passes it to init() — and calls it between a print() and the
// print_line() that completes the line.
var bootstrapLoadPrograms = map[string]string{
	"host": `_host_tick() ` + "`extern(\"promise_test_host_tick\") `wasm_import(\"promise_env\", \"tick\") `target(web)" + `;

main() {
    print("AB");
    _host_tick();
    print_line("CD");
}
`,
	"panic": `main() {
    assert(false, "boom");
}
`,
}

// bootstrapLoadDriver loads a generated bootstrap loader the way a page would
// (nodePageDriver). Arguments: the build output's base name, and how the page
// implements the program's own promise_env.tick import ("count", "throw",
// "write" to count ticks and also replace the base write, or "none" to pass no
// importObject). It reports one RESULT: line of JSON.
const bootstrapLoadDriver = nodePageDriver + `
const [name, mode] = process.argv.slice(2);

(async () => {
  const bytes = fs.readFileSync(path.join(dir, name + ".wasm"));
  const imports = WebAssembly.Module.imports(new WebAssembly.Module(bytes))
    .map((i) => i.module + "." + i.name).sort();
  const result = { imports, logs, errs, ticks: 0, written: 0 };
  // The page's own import: it counts its calls, or fails the way a DOM call
  // does — with a DOMException, whose numeric .code is not an exit code.
  const page = {
    count: { promise_env: { tick() { result.ticks++; } } },
    throw: { promise_env: { tick() { throw new DOMException("no such node", "NotFoundError"); } } },
    write: { promise_env: {
      tick() { result.ticks++; },
      write(fd, ptr, len) { result.written += Number(len); return len; },
    } },
  }[mode];
  const mod = await import(pathToFileURL(path.join(dir, name + ".js")).href);
  try {
    await mod.init(page);
    result.ok = true;
  } catch (e) {
    result.ok = false;
    result.exitSignal = e instanceof mod._ExitSignal;
    result.name = e && e.name;
    result.code = e && e.code;
  }
  process.stdout.write("RESULT:" + JSON.stringify(result) + "\n");
})();
`

type bootstrapLoadResult struct {
	Imports    []string `json:"imports"`
	Logs       []string `json:"logs"`
	Errs       []string `json:"errs"`
	Ticks      int      `json:"ticks"`
	Written    int      `json:"written"`
	OK         bool     `json:"ok"`
	ExitSignal bool     `json:"exitSignal"`
	Name       string   `json:"name"`
	Code       int      `json:"code"`
}

// TestBuildWebBootstrapLoadsProgram loads real compiler-produced wasm32-web
// binaries through the bootstrap loader `promise build` writes beside them
// (#305). The smoke tests in internal/bindgen run the loader against a
// hand-encoded module; this pins it to what the compiler actually imports, and
// to what a page can do with init():
//
//   - the binary imports the three base PAL entries and nothing else of its
//     own accord, so the bootstrap is all a program with no bindings needs;
//   - a page-supplied promise_env entry is reached through init()'s
//     importObject without displacing the base imports, and a print() is still
//     joined to the print_line() that follows it across that host call;
//   - a page-supplied entry named like a base one replaces it, so the page's
//     write receives the output and none reaches the console;
//   - an error a page import throws rejects init() as itself, not as an exit,
//     although a DOMException carries a numeric .code — the reason _ExitSignal
//     is exported — and the partial line is still flushed;
//   - a panic rejects init() with the exported _ExitSignal carrying code 1.
//
// It lives in package main, not tests/buildrun, because node must be the pinned
// runtime resolveWasmRuntime returns (T2169).
func TestBuildWebBootstrapLoadsProgram(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping build+node integration test in short mode")
	}
	nodePath, err := resolveWasmRuntime("node")
	if err != nil {
		// Neither staged on this machine nor fetchable: a fact about the
		// environment, not the tree.
		t.Skipf("the pinned node is not obtainable here: %v", err)
	}
	bin := clitest.Bin(t)
	dir := clitest.TempDir(t)

	outDir := filepath.Join(dir, "out")
	if err := os.MkdirAll(outDir, 0755); err != nil {
		t.Fatal(err)
	}
	for name, src := range bootstrapLoadPrograms {
		srcFile := filepath.Join(dir, name, "main.pr")
		if err := os.MkdirAll(filepath.Dir(srcFile), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(srcFile, []byte(src), 0644); err != nil {
			t.Fatal(err)
		}
		clitest.RunOK(t, bin, nil, "build", "-target", "wasm32-web", "-o", filepath.Join(outDir, name+".wasm"), srcFile)
	}
	driver := filepath.Join(outDir, "driver.cjs")
	if err := os.WriteFile(driver, []byte(bootstrapLoadDriver), 0644); err != nil {
		t.Fatal(err)
	}

	base := []string{"promise_env.exit", "promise_env.monotonic_nanos", "promise_env.write"}
	withTick := []string{"promise_env.exit", "promise_env.monotonic_nanos", "promise_env.tick", "promise_env.write"}
	cases := []struct {
		name, program, mode string
		want                bootstrapLoadResult
		wantErr             string // a substring of the console.error output
	}{
		{"page import", "host", "count", bootstrapLoadResult{
			Imports: withTick,
			Logs:    []string{"ABCD"},
			Ticks:   1,
			OK:      true,
		}, ""},
		{"page replaces a base import", "host", "write", bootstrapLoadResult{
			Imports: withTick,
			Ticks:   1,
			Written: len("AB") + len("CD\n"),
			OK:      true,
		}, ""},
		{"page import throws", "host", "throw", bootstrapLoadResult{
			Imports: withTick,
			Logs:    []string{"AB"},
			Name:    "NotFoundError",
			Code:    8,
		}, ""},
		{"panic", "panic", "none", bootstrapLoadResult{
			Imports:    base,
			ExitSignal: true,
			Code:       1,
		}, "boom"},
	}
	for _, c := range cases {
		got := runNodeDriver[bootstrapLoadResult](t, nodePath, driver, c.program, c.mode)
		if strings.Join(got.Imports, " ") != strings.Join(c.want.Imports, " ") {
			t.Errorf("%s: module imports = %q, want %q", c.name, got.Imports, c.want.Imports)
		}
		if got.OK != c.want.OK || got.ExitSignal != c.want.ExitSignal || got.Code != c.want.Code {
			t.Errorf("%s: init() ok=%v exitSignal=%v code=%d, want ok=%v exitSignal=%v code=%d",
				c.name, got.OK, got.ExitSignal, got.Code, c.want.OK, c.want.ExitSignal, c.want.Code)
		}
		// The rejection is the page's own error, not a wrapper of it.
		if c.want.Name != "" && got.Name != c.want.Name {
			t.Errorf("%s: init() rejected with a %q, want the page's %q", c.name, got.Name, c.want.Name)
		}
		if got.Ticks != c.want.Ticks {
			t.Errorf("%s: the page's tick import ran %d times, want %d", c.name, got.Ticks, c.want.Ticks)
		}
		if got.Written != c.want.Written {
			t.Errorf("%s: the page's write import received %d bytes, want %d", c.name, got.Written, c.want.Written)
		}
		if strings.Join(got.Logs, "\n") != strings.Join(c.want.Logs, "\n") {
			t.Errorf("%s: logs = %q, want %q", c.name, got.Logs, c.want.Logs)
		}
		if c.wantErr == "" && len(got.Errs) != 0 {
			t.Errorf("%s: unexpected stderr output %q", c.name, got.Errs)
		}
		if c.wantErr != "" && !strings.Contains(strings.Join(got.Errs, "\n"), c.wantErr) {
			t.Errorf("%s: stderr %q does not contain %q", c.name, got.Errs, c.wantErr)
		}
	}
}
