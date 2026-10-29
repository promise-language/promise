package bindgen

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/wasmweb"
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
	importSec = append(importSec, nameBytes(wasmweb.ImportModule)...)
	importSec = append(importSec, nameBytes("write")...)
	importSec = append(importSec, 0x00, 0x01)
	importSec = append(importSec, nameBytes(wasmweb.ImportModule)...)
	importSec = append(importSec, nameBytes("exit")...)
	importSec = append(importSec, 0x00, 0x00)
	importSec = append(importSec, nameBytes(wasmweb.ImportModule)...)
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
	exportSec = append(exportSec, nameBytes(wasmweb.ExportInitialize)...)
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

// importCallDriver returns a driver that instantiates the glue, keeping the
// import object init() hands to WebAssembly.instantiateStreaming as `imports`,
// then runs body, which calls those imports as the wasm module would — `mod`
// is the loader, for its _refStore and _refLoad, and `wasmExports` the
// instance's exports, its memory among them — and writes one "RESULT:" line.
func importCallDriver(body string) string {
	return `
const fs = require("fs");
const path = require("path");
const { pathToFileURL } = require("url");

const dir = __dirname;
const [loaderFile, ...initArgs] = process.argv.slice(2);

globalThis.fetch = async (url) => {
  const data = await fs.promises.readFile(path.join(dir, url));
  return new Response(data, { headers: { "content-type": "application/wasm" } });
};

let imports;
const instantiateStreaming = WebAssembly.instantiateStreaming;
WebAssembly.instantiateStreaming = (response, importObject) => {
  imports = importObject;
  return instantiateStreaming(response, importObject);
};

(async () => {
  const mod = await import(pathToFileURL(path.join(dir, loaderFile)).href);
  const wasmExports = await mod.init(...initArgs);
` + body + `})().catch((e) => {
  process.stdout.write("RESULT:ERR " + JSON.stringify(e && e.message) + "\n");
});
`
}

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
	modules := []*Module{{Name: "test", ImportModule: wasmweb.ImportModule}}
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
// name their keyword, never an empty member, and their bodies are property
// access (#696) — `_refLoad($handle).(...)` would make the whole file fail to
// load. (Duplicate object keys are legal JS, so key uniqueness is asserted on
// the text in overload_test.go, not here.)
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

// TestGenerateJSGlueReservedWordArgNamesSyntaxValid parses the glue for an
// operation whose arguments are named by every word a strict-mode ES module
// cannot bind (#675) — all but `void`, which WebIDL lexes as a type keyword and
// so cannot name an argument. Emitted verbatim, any one of them makes the whole
// module a SyntaxError.
func TestGenerateJSGlueReservedWordArgNamesSyntaxValid(t *testing.T) {
	nodePath := requireNode(t)
	src := `interface Target {
  undefined g(long await, long break, long case, long catch, long class,
    long const, long continue, long debugger, long default, long delete,
    long do, long else, long enum, long export, long extends, long false,
    long finally, long for, long function, long if, long import, long in,
    long instanceof, long new, long null, long return, long super,
    long switch, long this, long throw, long true, long try, long typeof,
    long var, long while, long with, long yield, long implements,
    long interface, long let, long package, long private, long protected,
    long public, long static, long arguments, long eval);
};`
	nodeCheckGlue(t, nodePath, GenerateJSGlue(overloadIR(t, src)))
}

// TestGenerateJSGlueArgNamesMatchingGlueParamsSyntaxValid parses the glue for
// operations whose arguments are named like a parameter the glue adds around
// them (#689): a method's receiver, a string result's return pointer, a string
// argument's pointer/length pair, and jsParamName's reserved-word escape. Each
// used to emit one parameter name twice, which method shorthand never allows, so
// the whole module was a SyntaxError.
func TestGenerateJSGlueArgNamesMatchingGlueParamsSyntaxValid(t *testing.T) {
	nodePath := requireNode(t)
	cases := []struct {
		name      string
		operation string
	}{
		{"receiver", "undefined f(long handle);"},
		{"return pointer", "DOMString g(long retptr);"},
		{"string pair", "undefined h(DOMString x, long xPtr, long xLen);"},
		{"reserved-word escape", "undefined k(long class, long class_);"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := "interface Target {\n  " + c.operation + "\n};"
			nodeCheckGlue(t, nodePath, GenerateJSGlue(overloadIR(t, src)))
		})
	}
}

// TestGenerateJSGlueArgNamesMatchingGlueParamsReachHost calls the imports of
// operations whose arguments are named like a parameter the glue adds (#689)
// and checks that each value reaches its own destination. node --check cannot:
// in `"Target.f"($handle, handle)` both names are valid expressions, so a body
// reading the receiver from the argument, or writing the string result through
// the `retptr` argument rather than the glue's return pointer, still parses.
//
// The receiver is a host object whose members record whether they were called
// on it, and what they received. Target.g's host member returns null, which the
// glue writes as the empty string (0, 0) without allocating; the driver fills
// the 8 bytes at the glue's return pointer (128) and at the `retptr` argument's
// value (64) with 0xFF first, so the write shows up at the one and not the
// other. Target.h's string argument is "AB", the first two bytes of the smoke
// module's data.
func TestGenerateJSGlueArgNamesMatchingGlueParamsReachHost(t *testing.T) {
	nodePath := requireNode(t)
	src := `interface Target {
  undefined f(long handle);
  DOMString g(long retptr);
  undefined h(DOMString x, long xPtr, long xLen);
  undefined k(long class, long class_);
};`
	loader := smokeLoader{name: "glue", js: GenerateJSGlue(overloadIR(t, src)), initArgs: []string{"frontend.wasm"}}
	result := runSmokeDriver(t, nodePath, loader, 0, importCallDriver(`
  const calls = [];
  const record = (member) => function (...args) { calls.push([member, this === target, ...args]); return null; };
  const target = { f: record("f"), g: record("g"), h: record("h"), k: record("k") };
  const self = mod._refStore(target);
  const bytes = new Uint8Array(wasmExports.memory.buffer);
  bytes.fill(0xff, 64, 72);
  bytes.fill(0xff, 128, 136);
  imports.promise_env["Target.f"](self, 42);
  imports.promise_env["Target.g"](self, 64, 128);
  imports.promise_env["Target.h"](self, 0, 2, 11, 12);
  imports.promise_env["Target.k"](self, 5, 6);
  process.stdout.write("RESULT:" + JSON.stringify({
    calls,
    returnPointer: Array.from(bytes.subarray(128, 136)),
    retptrArgument: Array.from(bytes.subarray(64, 72)),
  }) + "\n");
`))

	want := `RESULT:{"calls":[["f",true,42],["g",true,64],["h",true,"AB",11,12],["k",true,5,6]],` +
		`"returnPointer":[0,0,0,0,0,0,0,0],"retptrArgument":[255,255,255,255,255,255,255,255]}`
	if result != want {
		t.Errorf("Target's imports delivered\n%s\nwant\n%s", result, want)
	}
}

// TestGenerateJSGlueArgNamesMatchingGlueLocalsSyntaxValid parses the glue for
// arguments named like a local an import body declares (#690): the object a
// constructor creates, and a nullable interface result. Each body used to
// redeclare its parameter with `const`, so the whole module was a SyntaxError.
func TestGenerateJSGlueArgNamesMatchingGlueLocalsSyntaxValid(t *testing.T) {
	nodePath := requireNode(t)
	cases := []struct {
		name string
		src  string
	}{
		{"constructed object", "interface Target {\n  constructor(long obj);\n};"},
		{"nullable interface result", "interface Node {};\ninterface Target {\n  Node? f(long result);\n};"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			nodeCheckGlue(t, nodePath, GenerateJSGlue(overloadIR(t, c.src)))
		})
	}
}

// TestGenerateJSGlueArgNamesMatchingGlueLocalsReachHost calls the imports of a
// constructor and a nullable-interface operation whose arguments are named like
// the locals their bodies declare (#690). node --check cannot tell a body that
// stores its local from one that stores the argument of the same name: both
// parse, and the second hands the program a handle to a number.
//
// The driver defines the host class Target as a global, constructs one through
// the constructor import with 7, and calls f on it with 9, for which the host
// returns an object, and with 0, for which it returns null. It reports the
// constructed object's argument, the found object's, and the handle the null
// result crossed as.
func TestGenerateJSGlueArgNamesMatchingGlueLocalsReachHost(t *testing.T) {
	nodePath := requireNode(t)
	src := `interface Node {};
interface Target {
  constructor(long obj);
  Node? f(long result);
};`
	loader := smokeLoader{name: "glue", js: GenerateJSGlue(overloadIR(t, src)), initArgs: []string{"frontend.wasm"}}
	result := runSmokeDriver(t, nodePath, loader, 0, importCallDriver(`
  class Target {
    constructor(v) { this.v = v; }
    f(x) { return x === 0 ? null : { got: x }; }
  }
  globalThis.Target = Target;

  const env = imports.promise_env;
  const made = mod._refLoad(env["Target.constructor"](7));
  const self = mod._refStore(new Target(1));
  const found = mod._refLoad(env["Target.f"](self, 9));
  const absent = env["Target.f"](self, 0);
  process.stdout.write("RESULT:" + JSON.stringify({
    constructed: made instanceof Target ? made.v : String(made),
    found: found !== null && typeof found === "object" ? found.got : String(found),
    absent,
  }) + "\n");
`))

	if want := `RESULT:{"constructed":7,"found":9,"absent":0}`; result != want {
		t.Errorf("Target's imports delivered %s, want %s", result, want)
	}
}

// TestGenerateJSGlueReservedWordArgsReachHost calls the import of an operation
// whose arguments are named `null`, `arguments` and `eval` (#675) and checks the
// host member receives each value. node --check proves only that the glue
// parses, so it cannot tell which name a call forwards when that name is itself
// a valid expression: one of these three words left raw, or a renamed argument
// spelled differently from the parameter the import binds. Either way the host
// would get the wrong value (or a ReferenceError) at call time.
//
// The driver calls "Target.g" with a handle to a host object recording what its
// g member receives, then the arguments 7, a handle to a node, and 9, and
// reports the received arguments, the node named as "<node>".
func TestGenerateJSGlueReservedWordArgsReachHost(t *testing.T) {
	nodePath := requireNode(t)
	src := `interface Node {};
interface Target {
  undefined g(long null, Node arguments, long eval);
};`
	loader := smokeLoader{name: "glue", js: GenerateJSGlue(overloadIR(t, src)), initArgs: []string{"frontend.wasm"}}
	result := runSmokeDriver(t, nodePath, loader, 0, importCallDriver(`
  const node = { tag: "node" };
  let received;
  const target = { g(...args) { received = args; } };
  imports.promise_env["Target.g"](mod._refStore(target), 7, mod._refStore(node), 9);
  process.stdout.write("RESULT:" + JSON.stringify(received.map((v) => (v === node ? "<node>" : v))) + "\n");
`))

	if want := `RESULT:[7,"<node>",9]`; result != want {
		t.Errorf("Target.g's host member received %s, want %s", result, want)
	}
}

// TestGenerateJSGlueRedeclaredMemberReachesDeclaringInterface runs the imports
// of members a derived interface redeclares against a real JS engine (#682).
// Each must call P's own member on a D: resolved on the object, the getter
// reads D's "shadow", the setter throws (D's accessor has no setter and the
// glue is strict), m answers from D, and P's f(DOMString) reaches D's f, which
// declares only f(long). D's own m_2 still resolves on the object.
//
// The driver defines host classes P and D extends P as globals, where D
// redeclares P's v (getter only), m and f, and calls each import with a handle
// to a D. It reports which class's member answered: the getter's value, the
// setter's record (or the error the assignment threw), the "from" of each
// stored m result, and f's record — f's string argument read from the smoke
// module's data segment, "AB" at 0. P's members check their receiver as a
// host's do, so an import that calls P's member on anything but the object
// fails with "Illegal invocation".
func TestGenerateJSGlueRedeclaredMemberReachesDeclaringInterface(t *testing.T) {
	nodePath := requireNode(t)
	src := `interface P {
  attribute long v;
  P? m(long i);
  undefined f(long a);
  undefined f(DOMString s);
};
interface D : P {
  readonly attribute DOMString v;
  D? m(long i);
  undefined f(long a);
};`
	loader := smokeLoader{name: "glue", js: GenerateJSGlue(overloadIR(t, src)), initArgs: []string{"frontend.wasm"}}
	result := runSmokeDriver(t, nodePath, loader, 0, importCallDriver(`
  function brand(self) {
    if (!(self instanceof P)) throw new TypeError("Illegal invocation");
  }
  class P {
    get v() { brand(this); return 1; }
    set v(x) { brand(this); this.setBy = ["P", x]; }
    m(i) { brand(this); return { from: "P", i }; }
    f(x) { brand(this); this.calledBy = ["P", x]; }
  }
  class D extends P {
    get v() { return "shadow"; }
    m(i) { return { from: "D", i }; }
    f(x) { this.calledBy = ["D", x]; }
  }
  globalThis.P = P;
  globalThis.D = D;

  const env = imports.promise_env;
  const obj = new D();
  const handle = mod._refStore(obj);
  const got = {};
  got.get = env["P.v.get"](handle);
  try {
    env["P.v.set"](handle, 5);
    got.set = obj.setBy;
  } catch (e) {
    got.set = e.constructor.name;
  }
  got.m = mod._refLoad(env["P.m"](handle, 3)).from;
  got.m2 = mod._refLoad(env["D.m.2"](handle, 3)).from;
  env["P.f.2"](handle, 0, 2);
  got.f2 = obj.calledBy;
  process.stdout.write("RESULT:" + JSON.stringify(got) + "\n");
`))

	if want := `RESULT:{"get":1,"set":["P",5],"m":"P","m2":"D","f2":["P","AB"]}`; result != want {
		t.Errorf("imports reached %s, want %s", result, want)
	}
}

// TestGenerateJSGlueOverriddenBuiltInReachesPrototype runs the imports of a
// [LegacyOverrideBuiltIns] interface and of the one it inherits from against an
// object whose named properties hide their members (#697). Each must call the
// declaring interface's own member: resolved on the object, the getters read
// the "control" a named property holds, and the operations throw a TypeError,
// for the control is not callable. Form's [LegacyUnforgeable] u still resolves
// on the object, where it lives.
//
// The driver defines host classes Base and Form extends Base as globals, gives
// a Form own data properties v, m, w and n holding a control — the named
// properties a form's controls give it, over the prototype's members — and an
// own accessor u, then calls each import with a handle to it, reporting what it
// returned or the constructor of the error it threw. The members check their
// receiver as a host's do, so an import calling one on anything but the object
// fails with "Illegal invocation".
func TestGenerateJSGlueOverriddenBuiltInReachesPrototype(t *testing.T) {
	nodePath := requireNode(t)
	src := `interface Base {
  readonly attribute long v;
  long m(long i);
};
[LegacyOverrideBuiltIns] interface Form : Base {
  readonly attribute long w;
  long n(long i);
  [LegacyUnforgeable] readonly attribute long u;
};`
	loader := smokeLoader{name: "glue", js: GenerateJSGlue(overloadIR(t, src)), initArgs: []string{"frontend.wasm"}}
	result := runSmokeDriver(t, nodePath, loader, 0, importCallDriver(`
  function brand(self) {
    if (!(self instanceof Base)) throw new TypeError("Illegal invocation");
  }
  class Base {
    get v() { brand(this); return 1; }
    m(i) { brand(this); return i + 1; }
  }
  class Form extends Base {
    get w() { brand(this); return 2; }
    n(i) { brand(this); return i * 10; }
  }
  globalThis.Base = Base;
  globalThis.Form = Form;

  const env = imports.promise_env;
  const form = new Form();
  Object.defineProperty(form, "u", { get() { brand(this); return 3; }, enumerable: true });
  const control = { tag: "control" };
  for (const key of ["v", "m", "w", "n"]) {
    Object.defineProperty(form, key, { value: control, enumerable: true });
  }
  const handle = mod._refStore(form);
  const call = (f) => {
    try {
      return f();
    } catch (e) {
      return e.constructor.name;
    }
  };
  const got = {
    v: call(() => env["Base.v.get"](handle)),
    m: call(() => env["Base.m"](handle, 3)),
    w: call(() => env["Form.w.get"](handle)),
    n: call(() => env["Form.n"](handle, 3)),
    u: call(() => env["Form.u.get"](handle)),
  };
  process.stdout.write("RESULT:" + JSON.stringify(got) + "\n");
`))

	if want := `RESULT:{"v":1,"m":4,"w":2,"n":30,"u":3}`; result != want {
		t.Errorf("imports reached %s, want %s", result, want)
	}
}

// TestGenerateJSGlueAnonymousSpecialReachesHost calls the imports of
// specialOpsFixture's Target against a host object with indexed and named
// properties (#696). A legacy platform object is exotic, so a Proxy over a
// backing object stands in for one, its traps recording every key they see.
// String keys come from the smoke module's data segment, "ABCD\nEF" at offset 0:
// (0, 2) is "AB" and (2, 2) is "CD". An index of 2^31 crosses the boundary as
// the i32 -2147483648, and must still reach the host as that index.
func TestGenerateJSGlueAnonymousSpecialReachesHost(t *testing.T) {
	nodePath := requireNode(t)
	loader := smokeLoader{name: "glue", js: GenerateJSGlue(overloadIR(t, specialOpsFixture)), initArgs: []string{"frontend.wasm"}}
	result := runSmokeDriver(t, nodePath, loader, 0, importCallDriver(`
  const env = imports.promise_env;
  const first = { tag: "first" }, second = { tag: "second" }, third = { tag: "third" };
  const backing = { 0: first, AB: second };
  const keys = [];
  const target = new Proxy(backing, {
    get(obj, key) { keys.push(String(key)); return obj[key]; },
    set(obj, key, value) { keys.push(String(key)); obj[key] = value; return true; },
    deleteProperty(obj, key) { keys.push(String(key)); return delete obj[key]; },
  });
  const h = mod._refStore(target);
  const node = mod._refStore(third);
  const checks = {};
  checks.indexed_get = mod._refLoad(env["Target.getter"](h, 0)) === first;
  checks.indexed_get_absent = env["Target.getter"](h, 7) === 0;
  checks.named_get = mod._refLoad(env["Target.getter.2"](h, 0, 2)) === second;
  env["Target.setter"](h, 1, node);
  checks.indexed_set = backing[1] === third;
  env["Target.setter"](h, -2147483648, node);
  checks.indexed_set_high = backing["2147483648"] === third && !("-2147483648" in backing);
  env["Target.setter.2"](h, 2, 2, node);
  checks.named_set = backing.CD === third;
  env["Target.deleter"](h, 0, 2);
  checks.named_delete = !("AB" in backing);
  checks.keys_only = !keys.some((k) => k === "getter" || k === "setter" || k === "deleter");
  process.stdout.write("RESULT:" + JSON.stringify(checks) + "\n");
`))

	payload := strings.TrimPrefix(result, "RESULT:")
	if strings.HasPrefix(payload, "ERR ") {
		t.Fatalf("special-operation driver failed: %s", result)
	}
	var checks map[string]bool
	if err := json.Unmarshal([]byte(payload), &checks); err != nil {
		t.Fatalf("cannot parse driver result %q: %v", result, err)
	}
	for _, name := range []string{
		"indexed_get", "indexed_get_absent", "named_get", "indexed_set",
		"indexed_set_high", "named_set", "named_delete", "keys_only",
	} {
		if ok, reported := checks[name]; !reported {
			t.Errorf("driver did not report %q: %s", name, result)
		} else if !ok {
			t.Errorf("check %q does not hold", name)
		}
	}
}

// TestGenerateJSGlueHyphenatedMembersSyntaxValid parses the glue for members
// named with a `-` (#719). Dot access to one, `obj.margin-top = value`, is not
// an assignment target, so the whole module would be a SyntaxError.
func TestGenerateJSGlueHyphenatedMembersSyntaxValid(t *testing.T) {
	nodePath := requireNode(t)
	nodeCheckGlue(t, nodePath, GenerateJSGlue(overloadIR(t, hyphenGlueFixture)))
}

// TestGenerateJSGlueHyphenatedMembersReachHost calls the imports of
// hyphenGlueFixture against host objects whose members are named as the IDL
// spells them (#719). node --check cannot catch a dot-access getter or call:
// `obj.margin-top` parses as `obj.margin - top`. Target's `marginTop` and
// `margin-top` are distinct host properties, and each import must reach its
// own; Base's members, which Derived redeclares, must answer from Base on a
// Derived, and check their receiver as a host's do.
func TestGenerateJSGlueHyphenatedMembersReachHost(t *testing.T) {
	nodePath := requireNode(t)
	loader := smokeLoader{name: "glue", js: GenerateJSGlue(overloadIR(t, hyphenGlueFixture)), initArgs: []string{"frontend.wasm"}}
	result := runSmokeDriver(t, nodePath, loader, 0, importCallDriver(`
  function brand(self) {
    if (!(self instanceof Base)) throw new TypeError("Illegal invocation");
  }
  class Target {
    get marginTop() { return 1; }
    set marginTop(v) { this.setCamel = v; }
    get ["margin-top"]() { return 2; }
    set ["margin-top"](v) { this.setDashed = v; }
    ["do-it"](x) { return x * 10; }
    static ["make-it"]() { return 7; }
  }
  class Base {
    get ["page-orientation"]() { brand(this); return 3; }
    set ["page-orientation"](v) { brand(this); this.setBase = v; }
    ["run-it"](n) { brand(this); return n + 1; }
  }
  class Derived extends Base {
    get ["page-orientation"]() { return "shadow"; }
    ["run-it"](s) { return "shadow"; }
  }
  globalThis.Target = Target;
  globalThis.Base = Base;
  globalThis.Derived = Derived;

  const env = imports.promise_env;
  const target = new Target();
  const derived = new Derived();
  const t = mod._refStore(target);
  const d = mod._refStore(derived);
  const call = (f) => {
    try {
      return f();
    } catch (e) {
      return e.constructor.name;
    }
  };
  const got = {
    camel: call(() => env["Target.marginTop.get"](t)),
    dashed: call(() => env["Target.margin-top.get"](t)),
    doIt: call(() => env["Target.do-it"](t, 4)),
    makeIt: call(() => env["Target.make-it"]()),
    base: call(() => env["Base.page-orientation.get"](d)),
    run: call(() => env["Base.run-it"](d, 5)),
  };
  call(() => env["Target.marginTop.set"](t, 8));
  call(() => env["Target.margin-top.set"](t, 9));
  call(() => env["Base.page-orientation.set"](d, 6));
  got.setCamel = target.setCamel;
  got.setDashed = target.setDashed;
  got.setBase = derived.setBase;
  process.stdout.write("RESULT:" + JSON.stringify(got) + "\n");
`))

	want := `RESULT:{"camel":1,"dashed":2,"doIt":40,"makeIt":7,"base":3,"run":6,"setCamel":8,"setDashed":9,"setBase":6}`
	if result != want {
		t.Errorf("imports reached %s, want %s", result, want)
	}
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

// jsValueGlueIDL binds an interface taking and returning `any`, so the glue
// carries the JsValue runtime and imports (#173).
const jsValueGlueIDL = `interface Target {
  undefined take(any x);
  any give();
};`

// TestGenerateJSGlueJsValueSyntaxValid parses the glue of a module that has
// JsValue: its runtime, its imports and the _refTake argument reads.
func TestGenerateJSGlueJsValueSyntaxValid(t *testing.T) {
	nodePath := requireNode(t)
	nodeCheckGlue(t, nodePath, GenerateJSGlue(overloadIR(t, jsValueGlueIDL)))
}

// TestGenerateJSGlueJsValueReachesHost calls the JsValue imports as the
// generated conversions would (#173). It reports:
//   - the tag js_value_tag gives each kind of JS value — a bigint and a symbol
//     falling back to Object;
//   - the tag of each builder's value: a bool crossing as 1 or 0 is stored as a
//     JS boolean, so it lifts back as Bool, not Number;
//   - what the getters read back;
//   - what Target.take received for a built string ("AB", the smoke module's
//     first two bytes) and for an object handle;
//   - that the builder's temporary slot is released once the import has read
//     it, and handed out again by the next _refStore, while a handle a JsValue
//     holds stays loaded;
//   - that js_value_release clears its slot;
//   - that a host which throws on the argument still has its temporary slot
//     released, because _refTake reads it before the call.
//
// js_value_get_str is not called: writing a string result needs the module's
// cabi_realloc, which the smoke module does not export.
// TestBindWebIdlGlueRoundTripsJsValues (cmd/promise) runs it in a built module.
func TestGenerateJSGlueJsValueReachesHost(t *testing.T) {
	nodePath := requireNode(t)
	loader := smokeLoader{name: "glue", js: GenerateJSGlue(overloadIR(t, jsValueGlueIDL)), initArgs: []string{"frontend.wasm"}}
	result := runSmokeDriver(t, nodePath, loader, 0, importCallDriver(`
  const env = imports.promise_env;
  const received = [];
  const self = mod._refStore({ take(x) { received.push(x); } });
  const tags = [undefined, null, true, 1.5, "s", {}, [], () => {}, 10n, Symbol("q")]
    .map((v) => env.js_value_tag(mod._refStore(v)));
  const built = {
    undefined: env.js_value_undefined(),
    null: env.js_value_null(),
    true: env.js_value_bool(1),
    false: env.js_value_bool(0),
    number: env.js_value_number(2.5),
    str: env.js_value_str(0, 2),
  };
  const builtTags = {};
  for (const [k, h] of Object.entries(built)) builtTags[k] = env.js_value_tag(h);
  const got = [env.js_value_get_bool(built.true), env.js_value_get_bool(built.false), env.js_value_get_number(built.number)];

  env["Target.take"](self, built.str);
  const tempReleased = mod._refLoad(built.str) === undefined;
  const tempReused = mod._refStore("next") === built.str;
  const held = mod._refStore({ held: true });
  env["Target.take"](self, held);
  const heldKept = mod._refLoad(held) !== undefined;
  env.js_value_release(held);
  const releasedCleared = mod._refLoad(held) === undefined;

  const thrower = mod._refStore({ take() { throw new Error("host threw"); } });
  const doomed = env.js_value_str(0, 2);
  let threw = false;
  try { env["Target.take"](thrower, doomed); } catch (e) { threw = e.message === "host threw"; }
  const thrownReleased = mod._refLoad(doomed) === undefined;
  process.stdout.write("RESULT:" + JSON.stringify({ tags, builtTags, got, received, tempReleased, tempReused, heldKept, releasedCleared, threw, thrownReleased }) + "\n");
`))

	tag := func(name string) int { return jsValueTag(t, name) }
	want := fmt.Sprintf(`RESULT:{"tags":[%d,%d,%d,%d,%d,%d,%d,%d,%d,%d],`+
		`"builtTags":{"undefined":%d,"null":%d,"true":%d,"false":%d,"number":%d,"str":%d},`+
		`"got":[1,0,2.5],"received":["AB",{"held":true}],`+
		`"tempReleased":true,"tempReused":true,"heldKept":true,"releasedCleared":true,`+
		`"threw":true,"thrownReleased":true}`,
		tag("Undefined"), tag("Null"), tag("Bool"), tag("Number"), tag("Str"),
		tag("Object"), tag("Array"), tag("Function"), tag("Object"), tag("Object"),
		tag("Undefined"), tag("Null"), tag("Bool"), tag("Bool"), tag("Number"), tag("Str"))
	if result != want {
		t.Errorf("JsValue imports delivered\n%s\nwant\n%s", result, want)
	}
}
