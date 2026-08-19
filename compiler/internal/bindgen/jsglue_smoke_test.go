package bindgen

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requireNode skips the test if Node.js isn't on PATH — these tests exercise
// the generated glue with a real JS engine, so there's no meaningful fallback.
func requireNode(t *testing.T) string {
	t.Helper()
	nodePath, err := exec.LookPath("node")
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
// monotonic_nanos, matching what a Module with no resources or free
// functions still requires from the generated importObject. _initialize
// issues two PAL writes to fd 1, "AB" then "CD\n", as separate calls
// (exercising cross-write line buffering, since only the second call carries
// the newline) and then calls exit(exitCode).
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

	// Type section: 0: (i32)->() [exit], 1: (i32,i32,i32)->(i64) [write],
	// 2: ()->(i64) [monotonic_nanos], 3: ()->() [_initialize].
	typeSec := []byte{0x04}
	typeSec = append(typeSec, 0x60, 0x01, 0x7F, 0x00)
	typeSec = append(typeSec, 0x60, 0x03, 0x7F, 0x7F, 0x7F, 0x01, 0x7E)
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

	// Code section: _initialize body — write(1,0,2); drop; write(1,2,3);
	// drop; exit(exitCode).
	body := []byte{0x00} // no local decls
	body = append(body,
		0x41, 0x01, 0x41, 0x00, 0x41, 0x02, 0x10, 0x00, 0x1A, // write(1, 0, 2); drop
		0x41, 0x01, 0x41, 0x02, 0x41, 0x03, 0x10, 0x00, 0x1A, // write(1, 2, 3); drop
		0x41, exitCode, 0x10, 0x01, // exit(exitCode)
		0x0B, // end
	)
	codeSec := []byte{0x01}
	codeSec = append(codeSec, uleb(uint32(len(body)))...)
	codeSec = append(codeSec, body...)
	buf = append(buf, section(10, codeSec)...)

	// Data section: "ABCD\n" at offset 0, backing the two write() calls.
	data := []byte("ABCD\n")
	dataSeg := []byte{0x00, 0x41, 0x00, 0x0B} // active, memory 0, offset i32.const 0
	dataSeg = append(dataSeg, uleb(uint32(len(data)))...)
	dataSeg = append(dataSeg, data...)
	dataSec := []byte{0x01}
	dataSec = append(dataSec, dataSeg...)
	buf = append(buf, section(11, dataSec)...)

	return buf
}

// nodeSmokeDriver is a CommonJS Node script written next to the generated
// glue.js and its companion .wasm. It shims globalThis.fetch (Node's fetch
// has no support for relative or file:// URLs, unlike a browser) to read the
// sibling .wasm file, imports the generated ES module, and reports what
// happened on a single "RESULT:" line so the Go test can assert on it
// without a JS-side test framework.
const nodeSmokeDriver = `
const fs = require("fs");
const path = require("path");
const { pathToFileURL } = require("url");

const dir = __dirname;

globalThis.fetch = async (url) => {
  const data = await fs.promises.readFile(path.join(dir, url));
  return new Response(data, { headers: { "content-type": "application/wasm" } });
};

const logs = [];
const errs = [];
console.log = (...args) => { logs.push(args.join(" ")); };
console.error = (...args) => { errs.push(args.join(" ")); };

(async () => {
  const mod = await import(pathToFileURL(path.join(dir, "glue.js")).href);
  try {
    const exportsResult = await mod.init("frontend.wasm");
    process.stdout.write("RESULT:OK logs=" + JSON.stringify(logs) + " errs=" + JSON.stringify(errs) + " exports=" + JSON.stringify(Object.keys(exportsResult)) + "\n");
  } catch (e) {
    process.stdout.write("RESULT:ERR message=" + JSON.stringify(e && e.message) + " code=" + JSON.stringify(e && e.code) + " logs=" + JSON.stringify(logs) + " errs=" + JSON.stringify(errs) + "\n");
  }
})();
`

// runGlueSmoke generates glue for a bare "promise_env" module (no resources
// or free functions — the minimal shape that still needs the base PAL
// imports), writes it next to a hand-built smoke .wasm exiting with
// exitCode, runs the Node driver above against them, and returns its single
// "RESULT:..." stdout line.
func runGlueSmoke(t *testing.T, nodePath string, exitCode byte) string {
	t.Helper()
	dir := t.TempDir()

	modules := []*Module{{Name: "test", ImportModule: "promise_env"}}
	glueSource := GenerateJSGlue(modules)
	if err := os.WriteFile(filepath.Join(dir, "glue.js"), []byte(glueSource), 0644); err != nil {
		t.Fatalf("write glue.js: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "frontend.wasm"), buildSmokeWasmModule(exitCode), 0644); err != nil {
		t.Fatalf("write smoke wasm: %v", err)
	}
	driverFile := filepath.Join(dir, "driver.js")
	if err := os.WriteFile(driverFile, []byte(nodeSmokeDriver), 0644); err != nil {
		t.Fatalf("write driver: %v", err)
	}

	out, err := exec.Command(nodePath, driverFile).CombinedOutput()
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

// TestGenerateJSGlueSyntaxValid catches template bugs that a Go string
// builder can silently introduce (a stray brace, an unescaped quote) which
// no Go-side substring assertion would notice, but which would ship glue
// that fails to load in every browser.
func TestGenerateJSGlueSyntaxValid(t *testing.T) {
	nodePath := requireNode(t)
	dir := t.TempDir()
	modules := []*Module{{Name: "test", ImportModule: "promise_env"}}
	glueFile := filepath.Join(dir, "glue.mjs")
	if err := os.WriteFile(glueFile, []byte(GenerateJSGlue(modules)), 0644); err != nil {
		t.Fatalf("write glue: %v", err)
	}
	out, err := exec.Command(nodePath, "--check", glueFile).CombinedOutput()
	if err != nil {
		t.Fatalf("node --check reported invalid JS: %v\n%s", err, out)
	}
}

// TestGenerateJSGlueSmokeInstantiates is the direct rebuttal to "the catch is
// unreachable": before this fix, WebAssembly.instantiate threw a LinkError
// for any module (no promise_env.write/exit/monotonic_nanos in the generated
// importObject), so _initialize — and the exit-unwind catch around it — never
// ran. This instantiates the real generated glue against a real (if minimal)
// WASM module and checks that a clean exit(0) resolves init() instead of
// throwing, and that two writes without and then with a trailing newline are
// joined into a single console.log call rather than logging "AB", a blank
// line, and "CD" separately.
func TestGenerateJSGlueSmokeInstantiates(t *testing.T) {
	nodePath := requireNode(t)
	result := runGlueSmoke(t, nodePath, 0)

	if !strings.HasPrefix(result, "RESULT:OK") {
		t.Fatalf("expected instantiation + a clean exit(0) to resolve init(), got: %s", result)
	}
	if !strings.Contains(result, `logs=["ABCD"]`) {
		t.Errorf("expected the two writes to join into one buffered line \"ABCD\", got: %s", result)
	}
}

// TestGenerateJSGlueSmokeSurfacesNonZeroExit verifies that exit(1) — a panic
// or an explicit non-zero exit() — is not swallowed by the same catch that
// treats exit(0) as normal completion.
func TestGenerateJSGlueSmokeSurfacesNonZeroExit(t *testing.T) {
	nodePath := requireNode(t)
	result := runGlueSmoke(t, nodePath, 1)

	if !strings.HasPrefix(result, "RESULT:ERR") {
		t.Fatalf("expected exit(1) to reject init() instead of resolving, got: %s", result)
	}
	if !strings.Contains(result, `code=1`) {
		t.Errorf("expected the exit code to be preserved on the propagated error, got: %s", result)
	}
}
