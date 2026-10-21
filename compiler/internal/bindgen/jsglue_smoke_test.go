package bindgen

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requireNode skips the test if Node.js isn't on PATH — these tests exercise
// the generated loaders with a real JS engine, so there's no meaningful
// fallback.
func requireNode(t *testing.T) string {
	t.Helper()
	nodePath, err := exec.LookPath("node") // path-ok: skips for the absent runtime itself, which is the legitimate form
	if err != nil {
		t.Skip("node not found on PATH; skipping JS-glue smoke test")
	}
	return nodePath
}

// uleb encodes n as unsigned LEB128, the integer encoding the WASM binary
// format uses for section/vector lengths and indices.
func uleb(n uint32) []byte {
	var out []byte
	for {
		b := byte(n & 0x7F)
		n >>= 7
		if n != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if n == 0 {
			break
		}
	}
	return out
}

// buildSmokeWasmModule hand-encodes a minimal WASM binary — no LLVM/Promise
// toolchain involved — that imports exactly promise_env.write/exit/
// monotonic_nanos with the real ABI's shapes (codegen/pal/wasm_web.go), the
// base PAL imports every wasm32-web binary requires of either loader.
// _initialize calls monotonic_nanos, then issues three PAL writes to fd 1,
// "AB", "CD\n" and "EF", as separate calls — the first two exercise
// cross-write line buffering, since only the second carries the newline, and
// the third leaves a partial line for init() to flush once the run ends — and
// then calls exit(exitCode). write's len and monotonic_nanos's result are i64s,
// as in the real ABI, so they cross as BigInts (#305).
func buildSmokeWasmModule(exitCode byte) []byte {
	section := func(id byte, content []byte) []byte {
		out := []byte{id}
		out = append(out, uleb(uint32(len(content)))...)
		return append(out, content...)
	}
	nameBytes := func(s string) []byte {
		return append(uleb(uint32(len(s))), s...)
	}

	buf := []byte{0x00, 0x61, 0x73, 0x6D, 0x01, 0x00, 0x00, 0x00} // magic + version

	// Type section: 0: (i32)->() [exit], 1: (i32,i32,i64)->(i64) [write],
	// 2: ()->(i64) [monotonic_nanos], 3: ()->() [_initialize].
	typeSec := []byte{0x04}
	typeSec = append(typeSec, 0x60, 0x01, 0x7F, 0x00)
	typeSec = append(typeSec, 0x60, 0x03, 0x7F, 0x7F, 0x7E, 0x01, 0x7E)
	typeSec = append(typeSec, 0x60, 0x00, 0x01, 0x7E)
	typeSec = append(typeSec, 0x60, 0x00, 0x00)
	buf = append(buf, section(1, typeSec)...)

	// Import section: write (type 1) = func 0, exit (type 0) = func 1,
	// monotonic_nanos (type 2) = func 2.
	importSec := []byte{0x03}
	importSec = append(importSec, nameBytes("promise_env")...)
	importSec = append(importSec, nameBytes("write")...)
	importSec = append(importSec, 0x00, 0x01)
	importSec = append(importSec, nameBytes("promise_env")...)
	importSec = append(importSec, nameBytes("exit")...)
	importSec = append(importSec, 0x00, 0x00)
	importSec = append(importSec, nameBytes("promise_env")...)
	importSec = append(importSec, nameBytes("monotonic_nanos")...)
	importSec = append(importSec, 0x00, 0x02)
	buf = append(buf, section(2, importSec)...)

	// Function section: one defined function (_initialize, func index 3),
	// type 3.
	buf = append(buf, section(3, []byte{0x01, 0x03})...)

	// Memory section: one memory, min 1 page.
	buf = append(buf, section(5, []byte{0x01, 0x00, 0x01})...)

	// Export section: memory, and _initialize (func index 3).
	exportSec := []byte{0x02}
	exportSec = append(exportSec, nameBytes("memory")...)
	exportSec = append(exportSec, 0x02, 0x00)
	exportSec = append(exportSec, nameBytes("_initialize")...)
	exportSec = append(exportSec, 0x00, 0x03)
	buf = append(buf, section(7, exportSec)...)

	// Code section: _initialize body — monotonic_nanos(); drop;
	// write(1,0,2); drop; write(1,2,3); drop; write(1,5,2); drop;
	// exit(exitCode).
	body := []byte{0x00} // no local decls
	body = append(body,
		0x10, 0x02, 0x1A, // monotonic_nanos(); drop
		0x41, 0x01, 0x41, 0x00, 0x42, 0x02, 0x10, 0x00, 0x1A, // write(1, 0, 2 as i64); drop
		0x41, 0x01, 0x41, 0x02, 0x42, 0x03, 0x10, 0x00, 0x1A, // write(1, 2, 3 as i64); drop
		0x41, 0x01, 0x41, 0x05, 0x42, 0x02, 0x10, 0x00, 0x1A, // write(1, 5, 2 as i64); drop
		0x41, exitCode, 0x10, 0x01, // exit(exitCode)
		0x0B, // end
	)
	codeSec := []byte{0x01}
	codeSec = append(codeSec, uleb(uint32(len(body)))...)
	codeSec = append(codeSec, body...)
	buf = append(buf, section(10, codeSec)...)

	// Data section: "ABCD\nEF" at offset 0, backing the three write() calls.
	data := []byte("ABCD\nEF")
	dataSeg := []byte{0x00, 0x41, 0x00, 0x0B} // active, memory 0, offset i32.const 0
	dataSeg = append(dataSeg, uleb(uint32(len(data)))...)
	dataSeg = append(dataSeg, data...)
	dataSec := []byte{0x01}
	dataSec = append(dataSec, dataSeg...)
	buf = append(buf, section(11, dataSec)...)

	return buf
}

// nodeSmokeDriver is a CommonJS Node script written next to a generated
// loader and its companion .wasm, run as `node driver.js <loader> [init
// args...]`. It shims globalThis.fetch (Node's fetch has no support for
// relative or file:// URLs, unlike a browser) to read the sibling .wasm file,
// imports the loader as an ES module, calls its init() with the given
// arguments, and reports what happened on a single "RESULT:" line so the Go
// test can assert on it without a JS-side test framework.
const nodeSmokeDriver = `
const fs = require("fs");
const path = require("path");
const { pathToFileURL } = require("url");

const dir = __dirname;
const [loaderFile, ...initArgs] = process.argv.slice(2);

globalThis.fetch = async (url) => {
  const data = await fs.promises.readFile(path.join(dir, url));
  return new Response(data, { headers: { "content-type": "application/wasm" } });
};

const logs = [];
const errs = [];
console.log = (...args) => { logs.push(args.join(" ")); };
console.error = (...args) => { errs.push(args.join(" ")); };

(async () => {
  const mod = await import(pathToFileURL(path.join(dir, loaderFile)).href);
  try {
    const exportsResult = await mod.init(...initArgs);
    process.stdout.write("RESULT:OK logs=" + JSON.stringify(logs) + " errs=" + JSON.stringify(errs) + " exports=" + JSON.stringify(Object.keys(exportsResult)) + "\n");
  } catch (e) {
    process.stdout.write("RESULT:ERR message=" + JSON.stringify(e && e.message) + " code=" + JSON.stringify(e && e.code) + " exitSignal=" + JSON.stringify(e instanceof mod._ExitSignal) + " logs=" + JSON.stringify(logs) + " errs=" + JSON.stringify(errs) + "\n");
  }
})();
`

// refTableDriver instantiates the glue and then exercises the reference
// table directly through the module's exported _refStore/_refLoad/_refRelease.
// It reports one JSON object of named boolean checks so the Go side can name the
// exact invariant that broke rather than eyeballing a handle number (T1510).
const refTableDriver = `
const fs = require("fs");
const path = require("path");
const { pathToFileURL } = require("url");

const dir = __dirname;
const [loaderFile, ...initArgs] = process.argv.slice(2);

globalThis.fetch = async (url) => {
  const data = await fs.promises.readFile(path.join(dir, url));
  return new Response(data, { headers: { "content-type": "application/wasm" } });
};

(async () => {
  const mod = await import(pathToFileURL(path.join(dir, loaderFile)).href);
  await mod.init(...initArgs);
  const { _refStore, _refLoad, _refRelease } = mod;
  const checks = {};

  // Seeding is positional, not conditional: Node has no DOM, so slot 2 holds
  // undefined and console still lands in slot 3 rather than sliding into 2.
  checks.global_is_1 = _refLoad(1) === globalThis;
  checks.slot_2_absent_without_dom = _refLoad(2) === undefined;
  checks.console_is_3 = _refLoad(3) === console;

  // Pinned: a release of an ambient handle is ignored, so an owning wrapper
  // over a seeded handle cannot empty the slot or alias it to a later object.
  _refRelease(3);
  checks.console_survives_release = _refLoad(3) === console;
  const first = _refStore({ tag: "first" });
  checks.fresh_handle_above_pinned = first > 3;

  // A handle the program actually owns still round-trips through the free list.
  _refRelease(first);
  checks.released_slot_cleared = _refLoad(first) === undefined;
  checks.released_slot_reused = _refStore({ tag: "second" }) === first;

  process.stdout.write("RESULT:" + JSON.stringify(checks) + "\n");
})().catch((e) => {
  process.stdout.write("RESULT:ERR " + JSON.stringify(e && e.message) + "\n");
});
`

// smokeLoader is one of the two generated wasm32-web loaders. The base PAL
// runtime is one contract, so both run the same smoke module, driver and
// assertions (#305).
type smokeLoader struct {
	name     string
	js       string
	initArgs []string // what this loader's init() takes, passed through the driver's argv
}

// smokeLoaders returns both loaders, each set up to fetch frontend.wasm: the
// bootstrap has the path baked in, the glue takes it as init()'s argument.
func smokeLoaders() []smokeLoader {
	return []smokeLoader{
		{name: "bootstrap", js: GenerateBootstrapJS("frontend.wasm")},
		glueSmokeLoader(),
	}
}

// glueSmokeLoader is the glue for a bare "promise_env" module (no resources or
// free functions) — the minimal glue shape that still needs the base PAL
// imports.
func glueSmokeLoader() smokeLoader {
	modules := []*Module{{Name: "test", ImportModule: "promise_env"}}
	return smokeLoader{name: "glue", js: GenerateJSGlue(modules), initArgs: []string{"frontend.wasm"}}
}

// runSmokeDriver writes loader as loader.js next to a hand-built smoke .wasm
// exiting with exitCode, runs driver against them with the loader's init()
// arguments, and returns its single "RESULT:..." stdout line.
func runSmokeDriver(t *testing.T, nodePath string, loader smokeLoader, exitCode byte, driver string) string {
	t.Helper()
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "loader.js"), []byte(loader.js), 0644); err != nil {
		t.Fatalf("write loader.js: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "frontend.wasm"), buildSmokeWasmModule(exitCode), 0644); err != nil {
		t.Fatalf("write smoke wasm: %v", err)
	}
	driverFile := filepath.Join(dir, "driver.js")
	if err := os.WriteFile(driverFile, []byte(driver), 0644); err != nil {
		t.Fatalf("write driver: %v", err)
	}

	args := append([]string{driverFile, "loader.js"}, loader.initArgs...)
	out, err := exec.Command(nodePath, args...).CombinedOutput()
	result := ""
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "RESULT:") {
			result = line
		}
	}
	if result == "" {
		t.Fatalf("driver produced no RESULT line (err=%v):\n%s", err, out)
	}
	return result
}

// TestLoaderSyntaxValid catches template bugs that a Go string builder can
// silently introduce (a stray brace, an unescaped quote) which no Go-side
// substring assertion would notice, but which would ship a loader that fails
// to load in every browser.
func TestLoaderSyntaxValid(t *testing.T) {
	nodePath := requireNode(t)
	for _, loader := range smokeLoaders() {
		t.Run(loader.name, func(t *testing.T) {
			nodeCheckGlue(t, nodePath, loader.js)
		})
	}
}

// TestGenerateJSGlueOverloadSyntaxValid parses the glue generated for WebIDL
// overload sets (#263), including anonymous special operations: their imports
// must name a member, since `_refLoad(handle).(...)` would make the whole file
// fail to load. (Duplicate object keys are legal JS, so key uniqueness is
// asserted on the text in overload_test.go, not here.)
func TestGenerateJSGlueOverloadSyntaxValid(t *testing.T) {
	nodePath := requireNode(t)
	for _, src := range []string{overloadFixture, anonymousGetterFixture} {
		nodeCheckGlue(t, nodePath, GenerateJSGlue(overloadIR(t, src)))
	}
}

// TestGenerateJSGlueStringResultSyntaxValid parses the glue for string results
// (T1506): _writeStringResult and the trailing-retptr imports that call it.
func TestGenerateJSGlueStringResultSyntaxValid(t *testing.T) {
	nodePath := requireNode(t)
	nodeCheckGlue(t, nodePath, GenerateJSGlue(stringResultModules()))
}

// TestGenerateJSGlueKeywordArgNamesSyntaxValid parses the glue for an operation
// whose arguments are named by every WebIDL ArgumentNameKeyword (#650). The glue
// is a strict-mode module, where `interface` and `static` are reserved: emitted
// verbatim as parameter names, they make the whole module a SyntaxError.
func TestGenerateJSGlueKeywordArgNamesSyntaxValid(t *testing.T) {
	nodePath := requireNode(t)
	src := `interface Target {
  undefined f(long async, long attribute, long callback, long const,
    long constructor, long deleter, long dictionary, long enum, long getter,
    long includes, long inherit, DOMString interface, long iterable,
    long maplike, long mixin, long namespace, long partial, long readonly,
    long required, long setlike, long setter, long static, long stringifier,
    long typedef, long unrestricted);
};`
	nodeCheckGlue(t, nodePath, GenerateJSGlue(overloadIR(t, src)))
}

// nodeCheckGlue fails the test unless `node --check` parses glue.
func nodeCheckGlue(t *testing.T, nodePath, glue string) {
	t.Helper()
	glueFile := filepath.Join(t.TempDir(), "glue.mjs")
	if err := os.WriteFile(glueFile, []byte(glue), 0644); err != nil {
		t.Fatalf("write glue: %v", err)
	}
	out, err := exec.Command(nodePath, "--check", glueFile).CombinedOutput()
	if err != nil {
		t.Fatalf("node --check reported invalid JS: %v\n%s\n\nglue:\n%s", err, out, glue)
	}
}

// TestLoaderSmokeSuccessBuffersLines instantiates each loader against a real
// (if minimal) WASM module. For the glue it is the direct rebuttal to "the
// catch is unreachable": before T1506, WebAssembly.instantiate threw a
// LinkError for any module (no promise_env.write/exit/monotonic_nanos in the
// generated importObject), so _initialize — and the exit-unwind catch around
// it — never ran. It checks that a clean exit(0) resolves init() instead of
// throwing (which needs monotonic_nanos to return a BigInt), that two writes
// without and then with a trailing newline are joined into a single
// console.log call rather than logging "AB", a blank line, and "CD"
// separately, and that the trailing "EF" with no newline is still logged once
// the run ends.
func TestLoaderSmokeSuccessBuffersLines(t *testing.T) {
	nodePath := requireNode(t)
	for _, loader := range smokeLoaders() {
		t.Run(loader.name, func(t *testing.T) {
			result := runSmokeDriver(t, nodePath, loader, 0, nodeSmokeDriver)

			if !strings.HasPrefix(result, "RESULT:OK") {
				t.Fatalf("expected instantiation + a clean exit(0) to resolve init(), got: %s", result)
			}
			if !strings.Contains(result, `logs=["ABCD","EF"]`) {
				t.Errorf("expected the first two writes to join into one buffered line \"ABCD\" and the partial \"EF\" to be flushed at exit, got: %s", result)
			}
		})
	}
}

// TestLoaderSmokeSurfacesNonZeroExit verifies that exit(1) — a panic or an
// explicit non-zero exit() — is not swallowed by the same catch that treats
// exit(0) as normal completion, that it rejects init() with the loader's
// exported _ExitSignal carrying the code, and that the partial line pending
// when the run failed is flushed before the rejection rather than lost.
func TestLoaderSmokeSurfacesNonZeroExit(t *testing.T) {
	nodePath := requireNode(t)
	for _, loader := range smokeLoaders() {
		t.Run(loader.name, func(t *testing.T) {
			result := runSmokeDriver(t, nodePath, loader, 1, nodeSmokeDriver)

			if !strings.HasPrefix(result, "RESULT:ERR") {
				t.Fatalf("expected exit(1) to reject init() instead of resolving, got: %s", result)
			}
			if !strings.Contains(result, ` code=1 `) {
				t.Errorf("expected the exit code to be preserved on the propagated error, got: %s", result)
			}
			if !strings.Contains(result, `exitSignal=true`) {
				t.Errorf("expected the rejection to be an instance of the exported _ExitSignal, got: %s", result)
			}
			if !strings.Contains(result, `logs=["ABCD","EF"]`) {
				t.Errorf("expected the partial \"EF\" to be flushed before init() rejected, got: %s", result)
			}
		})
	}
}

// TestGenerateJSGlueRefTablePinsAmbientHandles is the runtime half of T1510:
// before the fix, init() seeded document/console conditionally (so handle 2 meant
// `console` in a DOM-less host) and _refRelease freed any handle, which let a
// wrapper that wrongly claimed to own a seeded slot both blank it and hand it to
// the next _refStore — silently aliasing `document` to an unrelated object. The
// per-handle checks below cover both halves plus the ordinary free-list reuse
// that pinning must not disturb.
func TestGenerateJSGlueRefTablePinsAmbientHandles(t *testing.T) {
	nodePath := requireNode(t)
	result := runSmokeDriver(t, nodePath, glueSmokeLoader(), 0, refTableDriver)

	payload := strings.TrimPrefix(result, "RESULT:")
	if strings.HasPrefix(payload, "ERR ") {
		t.Fatalf("ref-table driver failed: %s", result)
	}
	var checks map[string]bool
	if err := json.Unmarshal([]byte(payload), &checks); err != nil {
		t.Fatalf("cannot parse driver result %q: %v", result, err)
	}
	if len(checks) == 0 {
		t.Fatalf("driver reported no checks: %s", result)
	}
	for name, ok := range checks {
		if !ok {
			t.Errorf("ref-table invariant %q does not hold", name)
		}
	}
}
