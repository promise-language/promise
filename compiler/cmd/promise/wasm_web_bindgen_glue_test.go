package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
)

// T1506 end to end: a `promise bind webidl` module, built for wasm32-web, loads
// through the glue generated beside it with no hand-patching, and strings
// round-trip in both directions. Every earlier facet of T1506 had a test of its
// own glue text or of a hand-encoded module; what none of them could catch is
// the two generators disagreeing, which is the whole bug — the compiler declared
// a string result as a leading sret pointer to its boxed value struct while the
// glue returned a pointer into a cabi_retarea() export that does not exist, so
// the first string getter read the sret pointer as its handle.
//
// It lives in package main, not tests/buildrun, because node must be the pinned
// runtime resolveWasmRuntime returns: PATH is never consulted for it
// (docs/code-style.md §"Host tools in Go sources", T2169).

const bindgenGlueIDL = `interface Element {
  attribute DOMString innerHTML;
  attribute DOMString textContent;
  DOMString? getAttribute(DOMString name);
};
`

// Handle 1 is the ambient globalThis (T1510), so the driver stands in for the
// DOM by putting these members on the global object. The repeated reads reuse
// the allocator's free lists, where a host buffer released with the wrong
// allocator would corrupt the heap.
const bindgenGlueMain = `main() {
    e := Element.borrow(1);
    html := e.inner_html;
    print_line("html:" + html);
    e.inner_html = html + "!";
    print_line("id:[" + (e.get_attribute("id") ?: "none") + "]");
    print_line("missing:[" + (e.get_attribute("missing") ?: "none") + "]");
    print_line("undef:[" + (e.get_attribute("undef") ?: "none") + "]");
    first := e.text_content;
    stable := true;
    for i in 0..100 {
        if e.text_content != first {
            stable = false;
        }
    }
    print_line("text:" + first.len.to_string(base: 10) + " stable:" + stable.to_string());
}
`

// bindgenGlueDriver loads the generated element.js the way a page would
// (nodePageDriver). Arguments: the .wasm file, the textContent fixture
// ("empty" or "big"), and the innerHTML fixture ("ok" or "throw"). It reports
// one RESULT: line of JSON.
const bindgenGlueDriver = nodePageDriver + `
const [wasmFile, text, html] = process.argv.slice(2);

if (html === "throw") {
  Object.defineProperty(globalThis, "innerHTML", {
    get() { throw new Error("innerHTML getter failed"); },
    set(v) {},
    configurable: true,
  });
} else {
  globalThis.innerHTML = "héllo";
}
globalThis.textContent = text === "big" ? "x".repeat(200000) : "";
globalThis.getAttribute = (name) => (name === "id" ? "x1" : name === "undef" ? undefined : null);

(async () => {
  const mod = await import(pathToFileURL(path.join(dir, "element.js")).href);
  const result = { logs, errs };
  try {
    await mod.init(wasmFile);
    result.ok = true;
    result.innerHTML = globalThis.innerHTML;
  } catch (e) {
    result.ok = false;
    result.message = e && e.message;
  }
  process.stdout.write("RESULT:" + JSON.stringify(result) + "\n");
})();
`

type bindgenGlueResult struct {
	OK        bool     `json:"ok"`
	Message   string   `json:"message"`
	InnerHTML string   `json:"innerHTML"`
	Logs      []string `json:"logs"`
	Errs      []string `json:"errs"`
}

func TestBindWebIdlGlueRoundTripsStrings(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind+build+node integration test in short mode")
	}
	nodePath, err := resolveWasmRuntime("node")
	if err != nil {
		// Neither staged on this machine nor fetchable: a fact about the
		// environment, not the tree.
		t.Skipf("the pinned node is not obtainable here: %v", err)
	}
	bin := clitest.Bin(t)
	dir := clitest.TempDir(t)

	idlPath := filepath.Join(dir, "element.idl")
	if err := os.WriteFile(idlPath, []byte(bindgenGlueIDL), 0644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "out")
	clitest.RunOK(t, bin, nil, "bind", "webidl", "-name", "element", "-o", outDir, idlPath)
	if err := os.WriteFile(filepath.Join(outDir, "main.pr"), []byte(bindgenGlueMain), 0644); err != nil {
		t.Fatal(err)
	}
	driver := filepath.Join(outDir, "driver.cjs")
	if err := os.WriteFile(driver, []byte(bindgenGlueDriver), 0644); err != nil {
		t.Fatal(err)
	}

	// Debug builds run the debug allocator, which rejects a pal_free of a block
	// it did not hand out; release builds run the plain one under LTO.
	builds := []struct {
		name string
		args []string
	}{
		{"debug", nil},
		{"release", []string{"--release"}},
	}
	for _, b := range builds {
		wasm := b.name + ".wasm"
		args := append(append([]string{"build"}, b.args...), "-target", "wasm32-web", "-o", filepath.Join(outDir, wasm), outDir)
		clitest.RunOK(t, bin, nil, args...)

		cases := []struct {
			name, text, html string
			wantText         string
		}{
			{"empty", "empty", "ok", "text:0 stable:true"},
			// Larger than the allocator's biggest size class, so the host's
			// cabi_realloc grows memory inside the import: the (ptr, len) write
			// must go through a view taken after the growth.
			{"big", "big", "ok", "text:200000 stable:true"},
		}
		for _, c := range cases {
			got := runNodeDriver[bindgenGlueResult](t, nodePath, driver, wasm, c.text, c.html)
			name := b.name + "/" + c.name
			if !got.OK {
				t.Errorf("%s: init() rejected: %q (logs %q, errs %q)", name, got.Message, got.Logs, got.Errs)
				continue
			}
			want := []string{
				"html:héllo", // multi-byte UTF-8 result: the length is in bytes
				"id:[x1]",    // string param in, string result out
				"missing:[]", // a null DOMString? result is "" at the boundary (T0698)
				"undef:[]",   // so is undefined, not the string "undefined"
				c.wantText,
			}
			if strings.Join(got.Logs, "\n") != strings.Join(want, "\n") {
				t.Errorf("%s: logs = %q, want %q", name, got.Logs, want)
			}
			if got.InnerHTML != "héllo!" {
				t.Errorf("%s: innerHTML = %q after the setter, want %q", name, got.InnerHTML, "héllo!")
			}
			if len(got.Errs) != 0 {
				t.Errorf("%s: unexpected stderr output %q", name, got.Errs)
			}
		}

		// A host import that throws must reject init(), not be swallowed as a
		// normal exit-unwind.
		got := runNodeDriver[bindgenGlueResult](t, nodePath, driver, wasm, "empty", "throw")
		if got.OK || got.Message != "innerHTML getter failed" {
			t.Errorf("%s/throw: want init() rejected with the getter's error, got ok=%v message=%q", b.name, got.OK, got.Message)
		}
	}
}

// #168 end to end: the members a maplike and a setlike declaration bind reach a
// real JS Map and Set — the collections whose protocol WebIDL's maplike and
// setlike mirror — through the glue generated beside them, in both directions:
// the program reads back what it wrote, and the driver reads the host objects
// the program left behind.

const collectionsIDL = `interface Counts { maplike<DOMString, long>; };
interface Tags { setlike<DOMString>; };
interface Store {
  readonly attribute Counts counts;
  readonly attribute Tags tags;
};
`

// A missing key's get is not read: a built-in result crosses absent as its
// zero value (#752).
const collectionsMain = `main() {
    store := Store.borrow(1);
    c := store.counts;
    c._set("a", 1);
    c._set("b", -2);
    print_line("size:" + c.size.to_string(base: 10));
    print_line("get:" + (c._get("a") ?: 0i32).to_string(base: 10) + "," + (c._get("b") ?: 0i32).to_string(base: 10));
    print_line("has:" + c.has("b").to_string() + "," + c.has("z").to_string());
    print_line("delete:" + c.delete("b").to_string() + "," + c.delete("b").to_string());
    c.clear();
    print_line("cleared:" + c.size.to_string(base: 10));
    c._set("c", 3);

    t := store.tags;
    t.add("x");
    t.add("x");
    t.add("y");
    print_line("tags:" + t.size.to_string(base: 10));
    print_line("tags has:" + t.has("x").to_string() + "," + t.has("z").to_string());
    print_line("tags delete:" + t.delete("y").to_string() + "," + t.delete("y").to_string());
}
`

// collectionsDriver puts the Map and Set on the global object, handle 1 (T1510),
// loads the generated store.js the way a page would (nodePageDriver) and runs
// the .wasm named by its one argument. It reports one RESULT: line of JSON,
// with the collections' contents afterwards.
const collectionsDriver = nodePageDriver + `
const [wasmFile] = process.argv.slice(2);

globalThis.counts = new Map();
globalThis.tags = new Set();

(async () => {
  const mod = await import(pathToFileURL(path.join(dir, "store.js")).href);
  const result = { logs, errs };
  try {
    await mod.init(wasmFile);
    result.ok = true;
  } catch (e) {
    result.ok = false;
    result.message = e && e.message;
  }
  result.counts = JSON.stringify([...globalThis.counts]);
  result.tags = JSON.stringify([...globalThis.tags]);
  process.stdout.write("RESULT:" + JSON.stringify(result) + "\n");
})();
`

type collectionsResult struct {
	OK      bool     `json:"ok"`
	Message string   `json:"message"`
	Logs    []string `json:"logs"`
	Errs    []string `json:"errs"`
	Counts  string   `json:"counts"`
	Tags    string   `json:"tags"`
}

func TestBindWebIdlMaplikeSetlikeRoundTrip(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind+build+node integration test in short mode")
	}
	nodePath, err := resolveWasmRuntime("node")
	if err != nil {
		// Neither staged on this machine nor fetchable: a fact about the
		// environment, not the tree.
		t.Skipf("the pinned node is not obtainable here: %v", err)
	}
	bin := clitest.Bin(t)
	dir := clitest.TempDir(t)

	idlPath := filepath.Join(dir, "store.idl")
	if err := os.WriteFile(idlPath, []byte(collectionsIDL), 0644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "out")
	clitest.RunOK(t, bin, nil, "bind", "webidl", "-name", "store", "-o", outDir, idlPath)
	if err := os.WriteFile(filepath.Join(outDir, "main.pr"), []byte(collectionsMain), 0644); err != nil {
		t.Fatal(err)
	}
	driver := filepath.Join(outDir, "driver.cjs")
	if err := os.WriteFile(driver, []byte(collectionsDriver), 0644); err != nil {
		t.Fatal(err)
	}

	builds := []struct {
		name string
		args []string
	}{
		{"debug", nil},
		{"release", []string{"--release"}},
	}
	for _, b := range builds {
		wasm := b.name + ".wasm"
		args := append(append([]string{"build"}, b.args...), "-target", "wasm32-web", "-o", filepath.Join(outDir, wasm), outDir)
		clitest.RunOK(t, bin, nil, args...)

		got := runNodeDriver[collectionsResult](t, nodePath, driver, wasm)
		if !got.OK {
			t.Errorf("%s: init() rejected: %q (logs %q, errs %q)", b.name, got.Message, got.Logs, got.Errs)
			continue
		}
		want := []string{
			"size:2",
			"get:1,-2",
			"has:true,false",
			"delete:true,false", // a second delete finds nothing
			"cleared:0",
			"tags:2", // adding a value twice holds it once
			"tags has:true,false",
			"tags delete:true,false",
		}
		if strings.Join(got.Logs, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s: logs = %q, want %q", b.name, got.Logs, want)
		}
		if got.Counts != `[["c",3]]` || got.Tags != `["x"]` {
			t.Errorf("%s: host collections = %s and %s, want [[\"c\",3]] and [\"x\"]", b.name, got.Counts, got.Tags)
		}
		if len(got.Errs) != 0 {
			t.Errorf("%s: unexpected stderr output %q", b.name, got.Errs)
		}
	}
}

// #660 end to end: a program reaches the page through the generated
// `web.document` root, writing no handle number, and the root is the document
// the glue seeded. Dropping it frees nothing — were slot 2 freed,
// createElement's _refStore would reuse it and the second title read would see
// the element instead of the page. The glue's pin alone would keep the slot even
// under an owning wrapper, so that the root is borrowed is pinned by
// bindgen's TestCodegenAmbientRoots, not here.

const documentRootIDL = `interface Node { attribute DOMString textContent; };
interface Element : Node { attribute DOMString id; };
interface Document : Node {
  readonly attribute DOMString title;
  Element createElement(DOMString localName);
};
`

const documentRootMain = `use web "./web";

title_then_drop() string {
    page := web.document;
    return page.title;
}

main() {
    print_line("before:" + title_then_drop());
    made := web.document.create_element("div");
    made.id = "made";
    print_line("after:" + web.document.title);
}
`

// documentRootDriver stands in for the page's document before the glue is
// imported, because the glue seeds its ambient slots at module evaluation. It
// loads web/web.js the way a page would (nodePageDriver) and runs the .wasm
// named by its one argument, then probes the ref table the program left behind.
const documentRootDriver = nodePageDriver + `
const [wasmFile] = process.argv.slice(2);

const made = [];
globalThis.document = {
  title: "page",
  createElement(tag) {
    const el = { tag, id: "" };
    made.push(el);
    return el;
  },
};

(async () => {
  const mod = await import(pathToFileURL(path.join(dir, "web", "web.js")).href);
  const result = { logs, errs };
  try {
    await mod.init(wasmFile);
    result.ok = true;
  } catch (e) {
    result.ok = false;
    result.message = e && e.message;
  }
  result.slot2IsDocument = mod._refLoad(2) === globalThis.document;
  result.nextHandle = mod._refStore({ tag: "probe" });
  result.madeIds = made.map((el) => el.id);
  process.stdout.write("RESULT:" + JSON.stringify(result) + "\n");
})();
`

type documentRootResult struct {
	OK              bool     `json:"ok"`
	Message         string   `json:"message"`
	Logs            []string `json:"logs"`
	Errs            []string `json:"errs"`
	Slot2IsDocument bool     `json:"slot2IsDocument"`
	NextHandle      int      `json:"nextHandle"`
	MadeIDs         []string `json:"madeIds"`
}

func TestBindWebIdlDocumentRootIsSeededDocument(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind+build+node integration test in short mode")
	}
	nodePath, err := resolveWasmRuntime("node")
	if err != nil {
		// Neither staged on this machine nor fetchable: a fact about the
		// environment, not the tree.
		t.Skipf("the pinned node is not obtainable here: %v", err)
	}
	bin := clitest.Bin(t)
	dir := clitest.TempDir(t)

	idlPath := filepath.Join(dir, "dom.idl")
	if err := os.WriteFile(idlPath, []byte(documentRootIDL), 0644); err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(dir, "app")
	clitest.RunOK(t, bin, nil, "bind", "webidl", "-name", "web", "-o", filepath.Join(app, "web"), idlPath)
	for name, content := range map[string]string{
		"promise.toml": "[module]\nname = \"app\"\nepoch = \"" + bindEpoch() + "\"\n",
		"main.pr":      documentRootMain,
		"driver.cjs":   documentRootDriver,
	} {
		if err := os.WriteFile(filepath.Join(app, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	driver := filepath.Join(app, "driver.cjs")

	builds := []struct {
		name string
		args []string
	}{
		{"debug", nil},
		{"release", []string{"--release"}},
	}
	for _, b := range builds {
		wasm := b.name + ".wasm"
		args := append(append([]string{"build"}, b.args...), "-target", "wasm32-web", "-o", filepath.Join(app, wasm), app)
		clitest.RunOK(t, bin, nil, args...)

		got := runNodeDriver[documentRootResult](t, nodePath, driver, wasm)
		if !got.OK {
			t.Errorf("%s: init() rejected: %q (logs %q, errs %q)", b.name, got.Message, got.Logs, got.Errs)
			continue
		}
		want := []string{"before:page", "after:page"}
		if strings.Join(got.Logs, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s: logs = %q, want %q", b.name, got.Logs, want)
		}
		if !got.Slot2IsDocument {
			t.Errorf("%s: slot 2 no longer holds the seeded document after the root was dropped", b.name)
		}
		if got.NextHandle <= 3 {
			t.Errorf("%s: a fresh _refStore returned pinned handle %d; dropping a root must not free its slot", b.name, got.NextHandle)
		}
		if strings.Join(got.MadeIDs, ",") != "made" {
			t.Errorf("%s: created elements' ids = %q, want [made] (the owning wrapper beside the root)", b.name, got.MadeIDs)
		}
		if len(got.Errs) != 0 {
			t.Errorf("%s: unexpected stderr output %q", b.name, got.Errs)
		}
	}
}

// #170 end to end: a namespace binds as a type of `global` members named
// apart from it (console → Console), and each member's import reaches the host
// namespace object by its IDL name. Were the glue to name the object after the
// type, `Console.log` would throw a ReferenceError and init() would reject. The
// overload pins that both imports of a set call the one JS member.

const namespaceIDL = `namespace console {
  undefined log(DOMString message);
};
namespace probe {
  DOMString echo(DOMString s);
  DOMString echo(DOMString s, DOMString suffix);
};
`

const namespaceMain = `use web "./web";

main() {
    web.Console.log("hello");
    print_line(web.Probe.echo("a") + "|" + web.Probe.echo_2("a", "b"));
}
`

// namespaceDriver stands in for the page's probe namespace, loads web/web.js
// the way a page would (nodePageDriver), whose console.log it captures, and runs
// the .wasm named by its one argument.
const namespaceDriver = nodePageDriver + `
const [wasmFile] = process.argv.slice(2);

globalThis.probe = {
  echo(s, suffix) { return "echo:" + s + (suffix === undefined ? "" : suffix); },
};

(async () => {
  const mod = await import(pathToFileURL(path.join(dir, "web", "web.js")).href);
  const result = { logs, errs };
  try {
    await mod.init(wasmFile);
    result.ok = true;
  } catch (e) {
    result.ok = false;
    result.message = e && e.message;
  }
  process.stdout.write("RESULT:" + JSON.stringify(result) + "\n");
})();
`

func TestBindWebIdlNamespaceCallsHostObject(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind+build+node integration test in short mode")
	}
	nodePath, err := resolveWasmRuntime("node")
	if err != nil {
		// Neither staged on this machine nor fetchable: a fact about the
		// environment, not the tree.
		t.Skipf("the pinned node is not obtainable here: %v", err)
	}
	bin := clitest.Bin(t)
	dir := clitest.TempDir(t)

	idlPath := filepath.Join(dir, "namespaces.idl")
	if err := os.WriteFile(idlPath, []byte(namespaceIDL), 0644); err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(dir, "app")
	clitest.RunOK(t, bin, nil, "bind", "webidl", "-name", "web", "-o", filepath.Join(app, "web"), idlPath)
	for name, content := range map[string]string{
		"promise.toml": "[module]\nname = \"app\"\nepoch = \"" + bindEpoch() + "\"\n",
		"main.pr":      namespaceMain,
		"driver.cjs":   namespaceDriver,
	} {
		if err := os.WriteFile(filepath.Join(app, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	driver := filepath.Join(app, "driver.cjs")

	builds := []struct {
		name string
		args []string
	}{
		{"debug", nil},
		{"release", []string{"--release"}},
	}
	for _, b := range builds {
		wasm := b.name + ".wasm"
		args := append(append([]string{"build"}, b.args...), "-target", "wasm32-web", "-o", filepath.Join(app, wasm), app)
		clitest.RunOK(t, bin, nil, args...)

		got := runNodeDriver[bindgenGlueResult](t, nodePath, driver, wasm)
		if !got.OK {
			t.Errorf("%s: init() rejected: %q (logs %q, errs %q)", b.name, got.Message, got.Logs, got.Errs)
			continue
		}
		want := []string{"hello", "echo:a|echo:ab"}
		if strings.Join(got.Logs, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s: logs = %q, want %q", b.name, got.Logs, want)
		}
		if len(got.Errs) != 0 {
			t.Errorf("%s: unexpected stderr output %q", b.name, got.Errs)
		}
	}
}
