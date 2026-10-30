package main

import (
	"os"
	"path/filepath"
	"strconv"
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

// #173 end to end: a JsValue crosses between a bound module built for
// wasm32-web and the glue generated beside it, both ways. The two halves of the
// crossing — _js_value_to_ref/_js_value_from_ref and their externs in the
// module, the js_value_* imports in the glue — are rendered by two generators
// from one variant table, and each half's text has tests of its own; this is
// the test of them agreeing at runtime: each variant lifts as itself, a held
// object goes back to the host as the same object, and every primitive slot,
// going out or coming back, is released. An optional argument passed `none`
// reaches the host as `undefined` (#750), not as null.

const jsValueGlueIDL = `interface Thing {
  attribute any data;
  any echo(any x);
  any echoMaybe(optional any x);
  any make(DOMString kind);
  object? find(DOMString key);
};
`

// jsValueGlueRounds is how many times jsValueGlueMain sends each primitive
// variant through echo. Each crossing takes a slot going out (the builder's
// temporary) and one coming back (the result's), and both are released, so
// every round reuses the same slot; were either release missing for any one
// variant, jsValueGlueRounds slots would stay taken.
const jsValueGlueRounds = 100

// Handle 1 is the ambient globalThis (T1510), so the driver stands in for a
// Thing by putting its members on the global object.
var jsValueGlueMain = `describe(JsValue v) string {
    return match v {
        Undefined => "undefined",
        Null => "null",
        Bool(b) => "bool:{b}",
        Number(n) => "number:{n}",
        Str(s) => "str:{s}",
        Object(r) => "object",
        Array(r) => "array",
        Function(r) => "function",
    };
}

main() {
    t := Thing.borrow(1);
    held := t.data;
    print_line("data=" + describe(held));
    print_line("back=" + describe(t.echo(held)));
    for kind in ["array", "function", "bigint"] {
        print_line(kind + "=" + describe(t.make(kind)));
    }
    print_line("found=" + describe(t.find("yes") ?: JsValue.Str(value: "none")));
    print_line("missing=" + describe(t.find("no") ?: JsValue.Str(value: "none")));
    print_line("maybe-none=" + describe(t.echo_maybe(none)));
    print_line("maybe-null=" + describe(t.echo_maybe(JsValue.Null)));
    t.data = JsValue.Str(value: "set");
    for i in 0..` + strconv.Itoa(jsValueGlueRounds) + ` {
        for v in [JsValue.Undefined, JsValue.Null, JsValue.Bool(value: true), JsValue.Bool(value: false), JsValue.Number(value: 1.5), JsValue.Str(value: "héllo")] {
            got := describe(t.echo(v));
            if i == 0 {
                print_line(got);
            }
        }
        t.echo_maybe(none);
    }
}
`

// jsValueGlueDriver loads the generated thing.js the way a page would
// (nodePageDriver) and runs the .wasm named by its one argument. Besides the
// program's output it reports whether echo received the held object itself,
// what the data setter stored, and the handle a fresh _refStore gets once the
// program is done.
const jsValueGlueDriver = nodePageDriver + `
const [wasmFile] = process.argv.slice(2);

const held = { held: true };
let echoedHeld = false;
const made = { array: [1], function: () => 1, bigint: 10n };
globalThis.data = held;
globalThis.echo = (x) => {
  if (x === held) echoedHeld = true;
  return x;
};
globalThis.echoMaybe = (x) => x;
globalThis.make = (kind) => made[kind];
globalThis.find = (key) => (key === "yes" ? {} : null);

(async () => {
  const mod = await import(pathToFileURL(path.join(dir, "thing.js")).href);
  const result = { logs, errs };
  try {
    await mod.init(wasmFile);
    result.ok = true;
  } catch (e) {
    result.ok = false;
    result.message = e && e.message;
  }
  result.echoedHeld = echoedHeld;
  result.data = globalThis.data;
  result.nextHandle = mod._refStore({ probe: true });
  process.stdout.write("RESULT:" + JSON.stringify(result) + "\n");
})();
`

type jsValueGlueResult struct {
	OK         bool     `json:"ok"`
	Message    string   `json:"message"`
	Logs       []string `json:"logs"`
	Errs       []string `json:"errs"`
	EchoedHeld bool     `json:"echoedHeld"`
	Data       any      `json:"data"`
	NextHandle int      `json:"nextHandle"`
}

func TestBindWebIdlGlueRoundTripsJsValues(t *testing.T) {
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

	idlPath := filepath.Join(dir, "thing.idl")
	if err := os.WriteFile(idlPath, []byte(jsValueGlueIDL), 0644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "out")
	clitest.RunOK(t, bin, nil, "bind", "webidl", "-name", "thing", "-o", outDir, idlPath)
	if err := os.WriteFile(filepath.Join(outDir, "main.pr"), []byte(jsValueGlueMain), 0644); err != nil {
		t.Fatal(err)
	}
	driver := filepath.Join(outDir, "driver.cjs")
	if err := os.WriteFile(driver, []byte(jsValueGlueDriver), 0644); err != nil {
		t.Fatal(err)
	}

	// The debug allocator rejects a pal_free of a block it did not hand out,
	// such as a Str result's host-written bytes released the wrong way.
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

		got := runNodeDriver[jsValueGlueResult](t, nodePath, driver, wasm)
		if !got.OK {
			t.Errorf("%s: init() rejected: %q (logs %q, errs %q)", b.name, got.Message, got.Logs, got.Errs)
			continue
		}
		want := []string{
			"data=object",
			"back=object",
			"array=array",
			"function=function",
			"bigint=object", // a JS value no other variant takes lifts as Object
			"found=object",
			"missing=str:none", // a null object? result is none
			// An optional argument's none reaches the host as undefined, a
			// present Null as null.
			"maybe-none=undefined",
			"maybe-null=null",
			// The primitives echoed back: each built in the glue, lifted by tag.
			"undefined",
			"null",
			"bool:true", // a bool is stored as a JS boolean, so it lifts as Bool, not Number
			"bool:false",
			"number:1.5",
			"str:héllo", // multi-byte UTF-8 out through js_value_str and back through js_value_get_str
		}
		if strings.Join(got.Logs, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s: logs = %q, want %q", b.name, got.Logs, want)
		}
		if !got.EchoedHeld {
			t.Errorf("%s: echo did not receive the object data returned; a held Object must cross back as its own handle", b.name)
		}
		if got.Data != "set" {
			t.Errorf("%s: data = %#v after the setter, want the JS string \"set\"", b.name, got.Data)
		}
		if got.NextHandle >= jsValueGlueRounds {
			t.Errorf("%s: a fresh _refStore returned handle %d after %d rounds of primitive crossings; a primitive's slot is not being released", b.name, got.NextHandle, jsValueGlueRounds)
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

// #163 end to end: an optional or nullable interface argument crosses as its
// handle, or 0 for none, which the glue reads as `undefined` — at a method, an
// optional argument, a static, a setter and a constructor. Before #163 the
// wrapper passed the `Peer?` itself to the extern's i32, and bind's self-check
// rejected the module. The attribute takes the value back: the `undefined` a
// `none` became returns as 0 and lifts as `none`, and the global object as a
// handle to itself. An owned wrapper passed as an option is released after the
// call, as one passed bare is.

const optionalHandleIDL = `interface Peer {
  constructor(optional Peer owner);
  undefined take(Peer? p);
  undefined takeOptional(optional Peer p);
  static undefined takeStatic(Peer? p);
  attribute Peer? peer;
  readonly attribute Peer me;
  undefined hold(Peer? p);
};
`

// optionalHandleRounds is how many times optionalHandleMain passes each of two
// owned wrappers, a `Peer?` and a `Peer`, to a `Peer?` parameter. Each takes a
// slot from its getter and releases it when it drops, so every round reuses
// the same slots; were either kept, optionalHandleRounds slots would stay taken.
const optionalHandleRounds = 100

var optionalHandleMain = `presence(Peer? p) string {
    if p is present {
        return "present";
    }
    return "absent";
}

main() {
    host := Peer.borrow(1);
    host.take(none);
    host.take(Peer.borrow(1));
    host.take_optional(none);
    host.take_optional(Peer.borrow(1));
    Peer.take_static(none);
    Peer.take_static(Peer.borrow(1));
    host.peer = none;
    print_line("peer=" + presence(host.peer));
    host.peer = Peer.borrow(1);
    print_line("peer=" + presence(host.peer));
    host.take(host.peer);
    Peer.create(none);
    Peer.create(Peer.borrow(1));
    for _ in 0..` + strconv.Itoa(optionalHandleRounds) + ` {
        host.hold(host.peer);
        host.hold(host.me);
    }
}
`

// optionalHandleDriver puts the instance members on the global object, handle 1
// (T1510), and the static and the constructor on a Peer class. Each records
// what it received: "undefined", "global" for the global object, or its type.
// The peer attribute returns what it was last set to, me returns the global
// object, and hold counts the global objects it receives. It loads the
// generated peer.js the way a page would (nodePageDriver), runs the .wasm named
// by its one argument and reports one RESULT: line of JSON, with the handle a
// fresh _refStore gets once the program is done.
const optionalHandleDriver = nodePageDriver + `
const [wasmFile] = process.argv.slice(2);

const seen = [];
const record = (site, v) => {
  seen.push(site + ":" + (v === undefined ? "undefined" : v === globalThis ? "global" : typeof v));
};
globalThis.take = (p) => record("take", p);
globalThis.takeOptional = (p) => record("takeOptional", p);
let peer = null;
Object.defineProperty(globalThis, "peer", {
  get() { return peer; },
  set(v) { record("peer", v); peer = v; },
  configurable: true,
});
Object.defineProperty(globalThis, "me", {
  get() { return globalThis; },
  configurable: true,
});
let held = 0;
globalThis.hold = (p) => {
  if (p === globalThis) held++;
};
globalThis.Peer = class {
  constructor(owner) { record("constructor", owner); }
  static takeStatic(p) { record("takeStatic", p); }
};

(async () => {
  const mod = await import(pathToFileURL(path.join(dir, "peer.js")).href);
  const result = { logs, errs };
  try {
    await mod.init(wasmFile);
    result.ok = true;
  } catch (e) {
    result.ok = false;
    result.message = e && e.message;
  }
  result.seen = seen;
  result.held = held;
  result.nextHandle = mod._refStore({ probe: true });
  process.stdout.write("RESULT:" + JSON.stringify(result) + "\n");
})();
`

type optionalHandleResult struct {
	OK         bool     `json:"ok"`
	Message    string   `json:"message"`
	Logs       []string `json:"logs"`
	Errs       []string `json:"errs"`
	Seen       []string `json:"seen"`
	Held       int      `json:"held"`
	NextHandle int      `json:"nextHandle"`
}

func TestBindWebIdlGlueOptionalHandleParams(t *testing.T) {
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

	idlPath := filepath.Join(dir, "peer.idl")
	if err := os.WriteFile(idlPath, []byte(optionalHandleIDL), 0644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "out")
	clitest.RunOK(t, bin, nil, "bind", "webidl", "-name", "peer", "-o", outDir, idlPath)
	if err := os.WriteFile(filepath.Join(outDir, "main.pr"), []byte(optionalHandleMain), 0644); err != nil {
		t.Fatal(err)
	}
	driver := filepath.Join(outDir, "driver.cjs")
	if err := os.WriteFile(driver, []byte(optionalHandleDriver), 0644); err != nil {
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

		got := runNodeDriver[optionalHandleResult](t, nodePath, driver, wasm)
		if !got.OK {
			t.Errorf("%s: init() rejected: %q (logs %q, errs %q)", b.name, got.Message, got.Logs, got.Errs)
			continue
		}
		want := []string{
			"take:undefined", "take:global",
			"takeOptional:undefined", "takeOptional:global",
			"takeStatic:undefined", "takeStatic:global",
			"peer:undefined", "peer:global",
			"take:global", // the peer read back, passed on
			"constructor:undefined", "constructor:global",
		}
		if strings.Join(got.Seen, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s: the host received %q, want %q", b.name, got.Seen, want)
		}
		if wantLogs := []string{"peer=absent", "peer=present"}; strings.Join(got.Logs, "\n") != strings.Join(wantLogs, "\n") {
			t.Errorf("%s: logs = %q, want %q", b.name, got.Logs, wantLogs)
		}
		if got.Held != 2*optionalHandleRounds {
			t.Errorf("%s: hold received the global object %d times, want %d", b.name, got.Held, 2*optionalHandleRounds)
		}
		if got.NextHandle >= optionalHandleRounds {
			t.Errorf("%s: a fresh _refStore returned handle %d after %d rounds of owned wrappers passed as options; their slots are not being released", b.name, got.NextHandle, optionalHandleRounds)
		}
		if len(got.Errs) != 0 {
			t.Errorf("%s: unexpected stderr output %q", b.name, got.Errs)
		}
	}
}

// #766 end to end: a module consuming the bindings converts the Element
// createElement returns to the HTMLInputElement its host object is, through the
// generated checked conversion, and reads the input's value back; a div does
// not convert. The converted wrapper owns a handle of its own, so each of the
// two wrappers releases exactly one: none leaks across the rounds, none is
// released twice — a slot on the free list twice would come back from two
// fresh _refStore calls — and an input whose Element has already dropped still
// answers through its own handle.

const conversionIDL = `interface EventTarget {};
interface Node : EventTarget {};
interface Element : Node {};
interface HTMLElement : Element {};
interface HTMLInputElement : HTMLElement { attribute DOMString value; };
interface Document : Node { Element createElement(DOMString localName); };
`

// conversionRounds is how many times conversionMain converts an input and a
// div. Each round takes a slot for every Element and one for the converted
// input, and releases all of them, so every round reuses the same slots.
const conversionRounds = 100

var conversionMain = `use web "./web";

convert(string tag) string {
    made := web.document.create_element(tag);
    input := web.HTMLInputElement.from(made);
    if input {
        input.value = "typed-" + tag;
        return "input:" + input.value;
    }
    return "none:" + tag;
}

// The Element drops when this returns, before its conversion is used.
converted_input() web.HTMLInputElement? {
    made := web.document.create_element("input");
    return web.HTMLInputElement.from(made);
}

outlives() string {
    input := converted_input();
    if input {
        input.value = "kept";
        return input.value;
    }
    return "none";
}

main() {
    for _ in 0..` + strconv.Itoa(conversionRounds) + ` {
        convert("input");
        convert("div");
    }
    print_line(convert("input"));
    print_line(convert("div"));
    print_line(outlives());
}
`

// conversionDriver stands in for the page's DOM before the glue is imported,
// because the glue seeds the document's slot at module evaluation: host
// classes for the bound chain, and an HTMLDivElement the IDL does not bind. It
// loads web/web.js the way a page would (nodePageDriver) and runs the .wasm
// named by its first argument, the program converting its second argument's
// rounds. Once it is done it reports the values of the inputs the program
// made, how many slots above the three ambient ones still hold an object, and
// the handles twice that many rounds of fresh _refStore calls get. Each
// conversion round takes at most three slots and the three calls after them
// five, so the count's bound, two rounds past the last, covers every slot the
// program could have taken.
const conversionDriver = nodePageDriver + `
const [wasmFile, rounds] = process.argv.slice(2);

class EventTarget {}
class Node extends EventTarget {}
class Element extends Node {}
class HTMLElement extends Element {}
class HTMLInputElement extends HTMLElement {
  constructor() { super(); this.value = ""; }
}
class HTMLDivElement extends HTMLElement {}
Object.assign(globalThis, { EventTarget, Node, Element, HTMLElement, HTMLInputElement, HTMLDivElement });

const made = [];
globalThis.document = {
  createElement(tag) {
    const el = tag === "input" ? new HTMLInputElement() : new HTMLDivElement();
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
  result.inputValues = made.filter((el) => el instanceof HTMLInputElement).map((el) => el.value);
  result.live = 0;
  for (let h = 4; h < 4 + 3 * (Number(rounds) + 2); h++) {
    if (mod._refLoad(h) !== undefined) result.live++;
  }
  result.handles = [];
  for (let i = 0; i < 2 * Number(rounds); i++) result.handles.push(mod._refStore({ probe: i }));
  process.stdout.write("RESULT:" + JSON.stringify(result) + "\n");
})();
`

type conversionResult struct {
	OK          bool     `json:"ok"`
	Message     string   `json:"message"`
	Logs        []string `json:"logs"`
	Errs        []string `json:"errs"`
	InputValues []string `json:"inputValues"`
	Live        int      `json:"live"`
	Handles     []int    `json:"handles"`
}

func TestBindWebIdlConvertsToDerivedInterface(t *testing.T) {
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
	if err := os.WriteFile(idlPath, []byte(conversionIDL), 0644); err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(dir, "app")
	clitest.RunOK(t, bin, nil, "bind", "webidl", "-name", "web", "-o", filepath.Join(app, "web"), idlPath)
	for name, content := range map[string]string{
		"promise.toml": "[module]\nname = \"app\"\nepoch = \"" + bindEpoch() + "\"\n",
		"main.pr":      conversionMain,
		"driver.cjs":   conversionDriver,
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

		got := runNodeDriver[conversionResult](t, nodePath, driver, wasm, strconv.Itoa(conversionRounds))
		if !got.OK {
			t.Errorf("%s: init() rejected: %q (logs %q, errs %q)", b.name, got.Message, got.Logs, got.Errs)
			continue
		}
		if want := []string{"input:typed-input", "none:div", "kept"}; strings.Join(got.Logs, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s: logs = %q, want %q", b.name, got.Logs, want)
		}
		wantValues := make([]string, 0, conversionRounds+2)
		for range conversionRounds + 1 {
			wantValues = append(wantValues, "typed-input")
		}
		wantValues = append(wantValues, "kept")
		if strings.Join(got.InputValues, ",") != strings.Join(wantValues, ",") {
			t.Errorf("%s: the host inputs' values = %q, want %q: a value set through the converted wrapper must reach the input itself", b.name, got.InputValues, wantValues)
		}
		seen := map[int]bool{}
		for _, h := range got.Handles {
			if seen[h] {
				t.Errorf("%s: two fresh _refStore calls both returned handle %d: a converted or source wrapper released its handle twice", b.name, h)
				break
			}
			seen[h] = true
		}
		if got.Live != 0 {
			t.Errorf("%s: %d slots still hold an object after %d rounds of conversions; a converted or source wrapper is not releasing its handle", b.name, got.Live, conversionRounds)
		}
		if len(got.Errs) != 0 {
			t.Errorf("%s: unexpected stderr output %q", b.name, got.Errs)
		}
	}
}

// #766 end to end, where the conversion cannot be named `from`
// (docs/wasm-bindings.md §"Factory names"): Own declares a static `from` and P
// an instance one, which C inherits, so each of the three converts by
// `from_handle` — a `global `from` on C would override P's instance member,
// and bind's self-check would reject the module. H sees only its root's static
// `from`, which its conversion hides. Each conversion still reaches its
// `[cast]` import, and each `from` the IDL declares still reaches the host's.

const conversionFallbackIDL = `interface R { static undefined from(DOMString s); };
interface Own : R { static undefined from(DOMString s); };
interface P : R { undefined from(DOMString s); };
interface C : P {};
interface H : R {};
interface Document { R make(DOMString kind); };
`

const conversionFallbackMain = `use web "./web";

// The interfaces r's host object converts to, each followed by a ";".
convertible(web.R r) string {
    s := "";
    own := web.Own.from_handle(r);
    if own {
        s = s + "Own;";
    }
    p := web.P.from_handle(r);
    if p {
        p.from("via-p");
        s = s + "P;";
    }
    c := web.C.from_handle(r);
    if c {
        s = s + "C;";
    }
    h := web.H.from(r);
    if h {
        s = s + "H;";
    }
    return s;
}

main() {
    web.R.from("r");
    web.Own.from("own");
    print_line(convertible(web.document.make("c")));
    print_line(convertible(web.document.make("h")));
    print_line(convertible(web.document.make("own")));
}
`

// conversionFallbackDriver stands in for the page's host classes, whose `from`
// members log the call, and for the document that makes an object of the
// class its kind names, before loading web/web.js the way a page would
// (nodePageDriver) and running the .wasm named by its one argument.
const conversionFallbackDriver = nodePageDriver + `
const [wasmFile] = process.argv.slice(2);

class R { static from(s) { console.log("R.from " + s); } }
class Own extends R { static from(s) { console.log("Own.from " + s); } }
class P extends R { from(s) { console.log("P#from " + s); } }
class C extends P {}
class H extends R {}
Object.assign(globalThis, { R, Own, P, C, H });
globalThis.document = { make(kind) { return new ({ c: C, h: H, own: Own })[kind](); } };

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

func TestBindWebIdlConversionFallbackName(t *testing.T) {
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

	idlPath := filepath.Join(dir, "fallback.idl")
	if err := os.WriteFile(idlPath, []byte(conversionFallbackIDL), 0644); err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(dir, "app")
	clitest.RunOK(t, bin, nil, "bind", "webidl", "-name", "web", "-o", filepath.Join(app, "web"), idlPath)
	for name, content := range map[string]string{
		"promise.toml": "[module]\nname = \"app\"\nepoch = \"" + bindEpoch() + "\"\n",
		"main.pr":      conversionFallbackMain,
		"driver.cjs":   conversionFallbackDriver,
	} {
		if err := os.WriteFile(filepath.Join(app, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	clitest.RunOK(t, bin, nil, "build", "-target", "wasm32-web", "-o", filepath.Join(app, "app.wasm"), app)

	got := runNodeDriver[bindgenGlueResult](t, nodePath, filepath.Join(app, "driver.cjs"), "app.wasm")
	if !got.OK {
		t.Fatalf("init() rejected: %q (logs %q, errs %q)", got.Message, got.Logs, got.Errs)
	}
	want := []string{"R.from r", "Own.from own", "P#from via-p", "P;C;", "H;", "Own;"}
	if strings.Join(got.Logs, "\n") != strings.Join(want, "\n") {
		t.Errorf("logs = %q, want %q", got.Logs, want)
	}
	if len(got.Errs) != 0 {
		t.Errorf("unexpected stderr output %q", got.Errs)
	}
}
