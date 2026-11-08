package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/formatter"
	"github.com/promise-language/promise/compiler/internal/module"
)

// TestBindEpochMatchesCompiler verifies that generated bind module scaffolds
// (promise.toml for `promise bind wit`/`promise bind webidl`) carry the running
// compiler's epoch rather than a stale hardcoded literal (T0972). Both bind
// writers use bindEpoch(), so asserting the helper covers both call sites.
func TestBindEpochMatchesCompiler(t *testing.T) {
	t.Parallel()
	want, err := module.CompilerEpoch(embeddedCatalog)
	if err != nil || want == "" {
		want = "2026.1" // mirror bindEpoch's fallback
	}
	if got := bindEpoch(); got != want {
		t.Errorf("bindEpoch() = %q, want %q", got, want)
	}
	if got := bindEpoch(); got == "2026.0" && want != "2026.0" {
		t.Errorf("bindEpoch() returned stale hardcoded epoch 2026.0")
	}
}

// wantBindEpoch mirrors bindEpoch's resolution for use as the expected value in
// the end-to-end scaffold assertions below.
func wantBindEpoch(t *testing.T) string {
	t.Helper()
	want, err := module.CompilerEpoch(embeddedCatalog)
	if err != nil || want == "" {
		want = "2026.1"
	}
	return want
}

// assertScaffoldEpoch reads the promise.toml written into dir and checks that it
// advertises the running compiler's epoch and never the stale "2026.0" literal
// that T0972 fixed.
func assertScaffoldEpoch(t *testing.T, dir, want string) {
	t.Helper()
	toml, err := os.ReadFile(filepath.Join(dir, "promise.toml"))
	if err != nil {
		t.Fatalf("promise.toml not written: %v", err)
	}
	if !strings.Contains(string(toml), fmt.Sprintf("epoch = %q", want)) {
		t.Errorf("promise.toml epoch missing %q; got:\n%s", want, toml)
	}
	if want != "2026.0" && strings.Contains(string(toml), `epoch = "2026.0"`) {
		t.Errorf("promise.toml carries stale hardcoded epoch 2026.0")
	}
}

// silenceStdout redirects os.Stdout to /dev/null for the duration of fn — the
// bind writers print generated file paths we don't care about here.
func silenceStdout(t *testing.T, fn func()) {
	t.Helper()
	old := os.Stdout
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = devnull
	defer func() {
		os.Stdout = old
		devnull.Close()
	}()
	fn()
}

// TestRunBindWitScaffoldEpoch drives `promise bind wit` end-to-end on a minimal
// WIT package and asserts the generated promise.toml carries the running
// compiler's epoch (T0972) — covering the actual toml-write call site in
// runBindWit, not just the bindEpoch helper.
func TestRunBindWitScaffoldEpoch(t *testing.T) {
	dir := t.TempDir()
	witPath := filepath.Join(dir, "thing.wit")
	if err := os.WriteFile(witPath, []byte("package example:thing@0.1.0;\n"), 0644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "out")

	silenceStdout(t, func() {
		runBindWit([]string{"-o", outDir, witPath})
	})

	assertScaffoldEpoch(t, outDir, wantBindEpoch(t))
}

// TestRunBindWebIdlScaffoldEpoch drives `promise bind webidl` end-to-end on a
// minimal WebIDL interface and asserts the generated promise.toml carries the
// running compiler's epoch (T0972) — covering runBindWebIdl's toml-write call
// site.
func TestRunBindWebIdlScaffoldEpoch(t *testing.T) {
	dir := t.TempDir()
	idlPath := filepath.Join(dir, "widget.webidl")
	if err := os.WriteFile(idlPath, []byte("interface Widget {};\n"), 0644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "out")

	silenceStdout(t, func() {
		runBindWebIdl([]string{"-o", outDir, idlPath})
	})

	assertScaffoldEpoch(t, outDir, wantBindEpoch(t))
}

// TestRunBindWebIdlModuleName: `bind webidl` names the module by -name, or else
// by its first input file, as given, without its `.idl` or `.webidl` suffix and
// with `-` written `_` — never by an interface, whether the first one is named
// Web or anything else (#60; docs/wasm-bindings.md §"The bind Subcommand").
func TestRunBindWebIdlModuleName(t *testing.T) {
	cases := []struct {
		name  string
		files []string // the first declares iface, each other one an interface of its own
		iface string
		args  []string
		want  string
	}{
		{name: "the first input file", files: []string{"other.idl"}, iface: "Document", want: "other"},
		{name: "a first interface named Web", files: []string{"other.idl"}, iface: "Web", want: "other"},
		{name: "-name", files: []string{"other.idl"}, iface: "Document", args: []string{"-name", "web"}, want: "web"},
		{name: "a hyphenated file", files: []string{"css-anchor.idl"}, iface: "Document", want: "css_anchor"},
		{name: "a .webidl file", files: []string{"dom.webidl"}, iface: "Document", want: "dom"},
		{name: "the first of several", files: []string{"zeta.idl", "alpha.idl"}, iface: "Document", want: "zeta"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			var paths []string
			for i, file := range tc.files {
				iface := tc.iface
				if i > 0 {
					iface = fmt.Sprintf("Extra%d", i)
				}
				path := filepath.Join(dir, file)
				idl := "[Exposed=Window]\ninterface " + iface + " { readonly attribute long n; };\n"
				if err := os.WriteFile(path, []byte(idl), 0644); err != nil {
					t.Fatal(err)
				}
				paths = append(paths, path)
			}
			outDir := filepath.Join(dir, "out")

			silenceStdout(t, func() {
				runBindWebIdl(append(append([]string{"-o", outDir}, tc.args...), paths...))
			})

			for _, ext := range []string{".pr", ".js"} {
				if _, err := os.Stat(filepath.Join(outDir, tc.want+ext)); err != nil {
					t.Errorf("module file not written under the module name: %v", err)
				}
			}
			toml, err := os.ReadFile(filepath.Join(outDir, "promise.toml"))
			if err != nil {
				t.Fatalf("promise.toml not written: %v", err)
			}
			if want := fmt.Sprintf("name = %q", tc.want); !strings.Contains(string(toml), want) {
				t.Errorf("promise.toml missing %s; got:\n%s", want, toml)
			}
		})
	}
}

// assertFormattedModule fails unless the module bind wrote to outDir/name.pr
// is already in `promise format`'s canonical form, which is the comparison
// `promise format -check` makes (runFmt). Such a file passes that check
// unedited (#783; docs/wasm-bindings.md §"The bind Subcommand").
func assertFormattedModule(t *testing.T, outDir, name string) {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(outDir, name+".pr"))
	if err != nil {
		t.Fatalf("module not written: %v", err)
	}
	formatted := formatter.Format(src)
	if bytes.Equal(formatted, src) {
		return
	}
	written := strings.Split(string(src), "\n")
	want := strings.Split(string(formatted), "\n")
	for i := range min(len(written), len(want)) {
		if written[i] != want[i] {
			t.Fatalf("%s.pr is not formatted: promise format -check rejects it. First difference, at line %d:\n  written:   %q\n  formatted: %q", name, i+1, written[i], want[i])
		}
	}
	t.Fatalf("%s.pr is not formatted: promise format -check rejects it. It has %d lines, and formatted it has %d", name, len(written), len(want))
}

// formattedReproIDL is #783's repro: the smallest module bind generates.
const formattedReproIDL = `[Exposed=Window]
interface A {
  attribute DOMString label;
};
`

// formattedConstructsIDL generates what the glue fixtures do not: a string
// enum, a dictionary with an enum-typed member and one inheriting from it, an
// overloaded operation, a constant, a constructor and a static operation. It
// leaves out an enum-typed attribute and a dictionary-typed argument: neither
// lowers yet (#175, #177), so the self-check would reject the module for that
// rather than for its layout.
const formattedConstructsIDL = `enum Mode { "open", "closed" };
dictionary Opts {
  Mode mode;
  boolean bubbles = false;
  long detail = 0;
};
dictionary MoreOpts : Opts { DOMString label; };
[Exposed=Window]
interface Shadow {
  constructor(DOMString name);
  undefined setAttribute(DOMString name, DOMString value);
  undefined setAttribute(DOMString name, long value);
  const unsigned short ELEMENT_NODE = 1;
  static Shadow make(DOMString kind);
  readonly attribute unsigned long length;
};
`

// formattedResultsIDL generates the result branches no other fixture reaches:
// a JsValue returned by a namespace operation, which binds as a static, and a
// nullable JsValue returned by a static and by an attribute, and a nullable
// handle returned by a method, whose wrappers return `none` from a one-line
// `if`.
const formattedResultsIDL = `[Exposed=Window]
interface Node {
  Node? find(DOMString selector);
  static (DOMString or long)? lookup(DOMString key);
  readonly attribute (DOMString or long)? value;
};
[Exposed=Window]
namespace Tools {
  (DOMString or long) get(DOMString key);
};
`

// TestRunBindWebIdlWritesFormattedModule: every module `bind webidl` writes
// passes `promise format -check` as written, whatever constructs it generates
// (JsValue's match helpers, nullable handles, namespaces, overloads, the from
// conversion, dictionaries, enums), with a keep list, and with the self-check
// skipped (#783; docs/wasm-bindings.md §"The bind Subcommand"). Every other
// case runs the self-check, so each one also proves that the formatted module
// still type-checks.
func TestRunBindWebIdlWritesFormattedModule(t *testing.T) {
	cases := []struct {
		name string
		idl  string
		keep string // a keep list, when the case binds a subset
		args []string
	}{
		{name: "the #783 repro", idl: formattedReproIDL},
		{name: "the #783 repro without the self-check", idl: formattedReproIDL, args: []string{"-no-check"}},
		{name: "string attributes and a nullable result", idl: bindgenGlueIDL},
		{name: "JsValue", idl: jsValueGlueIDL},
		{name: "namespaces and overloads", idl: namespaceIDL},
		{name: "the from conversion", idl: conversionIDL},
		{name: "the from conversion beside a from member", idl: conversionFallbackIDL},
		{name: "optional handles", idl: optionalHandleIDL},
		{name: "nullable scalars", idl: nullableScalarIDL},
		{name: "maplike and setlike", idl: collectionsIDL},
		{name: "document root", idl: documentRootIDL},
		{name: "dictionaries, enums, overloads, constants and statics", idl: formattedConstructsIDL},
		{name: "a keep list", idl: formattedConstructsIDL, keep: "Shadow\nShadow.constructor\nShadow.setAttribute\n"},
		{name: "JsValue and nullable results on statics, attributes and methods", idl: formattedResultsIDL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			idlPath := filepath.Join(dir, "dom.idl")
			if err := os.WriteFile(idlPath, []byte(tc.idl), 0644); err != nil {
				t.Fatal(err)
			}
			outDir := filepath.Join(dir, "out")
			args := append([]string{"-name", "dom", "-o", outDir}, tc.args...)
			if tc.keep != "" {
				keepPath := filepath.Join(dir, "dom.keep")
				if err := os.WriteFile(keepPath, []byte(tc.keep), 0644); err != nil {
					t.Fatal(err)
				}
				args = append(args, "-keep", keepPath)
			}

			silenceStdout(t, func() {
				runBindWebIdl(append(args, idlPath))
			})

			assertFormattedModule(t, outDir, "dom")
		})
	}
}

// formattedConstructsWIT generates the WIT types (enum, record, variant,
// flags), a resource with a constructor, a method and an optional result, and
// free functions taking a string and a list. It leaves out what does not yet
// generate a module that parses or type-checks: `result<...>` (#419, #543), a
// keyword name such as a `type` field (#794), a free function returning a
// resource (#421), a `type` alias (#726) and `own`/`borrow` parameters
// (#413).
const formattedConstructsWIT = `package example:constructs@0.1.0;

interface types {
    enum color {
        red,
        dark-green,
    }

    record point {
        x: s32,
        y: s32,
        tint: color,
    }

    variant shape {
        circle(f64),
        empty,
    }

    flags open-flags {
        create,
        truncate,
    }

    resource node {
        constructor(name: string);
        rename: func(name: string);
        label: func() -> option<string>;
    }

    checksum: func(data: list<u8>) -> u32;
    greet: func(name: string) -> string;
}
`

// formattedCanonicalWIT is the free-function shape the canonical ABI lowers
// today: string and list arguments and results, through the retarea. Its
// resource members and its option and tuple results do not lower yet (#541,
// #542).
const formattedCanonicalWIT = `package example:api;

interface greet {
  hello: func(name: string) -> string;
  checksum: func(data: list<u8>) -> u32;
  payload: func() -> list<u8>;
}

world api {
  import greet;
}
`

// formattedUncheckedWIT is what formattedConstructsWIT and formattedCanonicalWIT
// leave out because the self-check rejects it, short of a keyword name (#794),
// whose module does not parse: a documented `type` alias, which generates a
// comment (#726), `own` and `borrow` parameters (#413), a free function
// returning a resource (#421), `result<...>` results, whose canonical lift
// raises from a one-line `if` (#419, #543), and option and tuple results
// through the retarea (#542). Bound with -no-check, the module is written all
// the same, so it is formatted all the same.
const formattedUncheckedWIT = `package example:unchecked@0.1.0;

interface api {
    /// A byte count.
    type size = u64;

    enum errno {
        access,
        busy,
    }

    resource node {
        constructor(name: string);
        adopt: func(child: own<node>);
        peek: func(other: borrow<node>) -> u32;
    }

    open: func(path: string) -> node;
    stat: func(path: string) -> result<size, errno>;
    remove: func(path: string) -> result<_, errno>;
    pair: func() -> tuple<u32, string>;
    maybe: func() -> option<string>;
}

world unchecked {
  import api;
}
`

// TestRunBindWitWritesFormattedModule: every module `bind wit` writes passes
// `promise format -check` as written, with or without the canonical ABI, and
// with the self-check skipped (#783; docs/wasm-bindings.md §"The bind
// Subcommand"). Every case that runs the self-check also proves that the
// formatted module still type-checks.
func TestRunBindWitWritesFormattedModule(t *testing.T) {
	cases := []struct {
		name string
		wit  string
		args []string
	}{
		{name: "one free function", wit: "package example:thing@0.1.0;\n\ninterface api {\n    hello: func(name: string) -> string;\n}\n"},
		{name: "types, a resource and free functions", wit: formattedConstructsWIT},
		{name: "the canonical ABI", wit: formattedCanonicalWIT, args: []string{"-canonical-abi"}},
		{name: "a module the self-check rejects, bound without it", wit: formattedUncheckedWIT, args: []string{"-no-check"}},
		{name: "a module the self-check rejects, bound without it, with the canonical ABI", wit: formattedUncheckedWIT, args: []string{"-no-check", "-canonical-abi"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			witPath := filepath.Join(dir, "api.wit")
			if err := os.WriteFile(witPath, []byte(tc.wit), 0644); err != nil {
				t.Fatal(err)
			}
			outDir := filepath.Join(dir, "out")

			silenceStdout(t, func() {
				runBindWit(append(append([]string{"-name", "api", "-o", outDir}, tc.args...), witPath))
			})

			assertFormattedModule(t, outDir, "api")
		})
	}
}

// selfCheckDirEnv hands the child of TestRunBindSelfCheckChecksTheHost the
// project it self-checks: the self-check leaves through os.Exit when the
// project does not resolve, so the failing case runs it in a child.
const selfCheckDirEnv = "TEST_BIND_SELF_CHECK_DIR"

// writeSelfCheckProject writes a project in the shape bind generates: a
// `target(web) wrapper Node and a type D naming it, whose declaration carries
// dTarget (empty, or " `target(web)").
func writeSelfCheckProject(t *testing.T, dTarget string) string {
	t.Helper()
	dir := t.TempDir()
	src := "type Node `structural(protocol: false) `public `target(web) `doc(\"A wrapper.\") {\n" +
		"    i32 _handle;\n" +
		"}\n\n" +
		"type D `structural(protocol: false) `public" + dTarget + " `doc(\"A dictionary.\") {\n" +
		"    Node n;\n" +
		"}\n"
	if err := os.WriteFile(filepath.Join(dir, "dom.pr"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	toml := fmt.Sprintf("[module]\nname = \"dom\"\nepoch = %q\n", bindEpoch())
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"), []byte(toml), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestRunBindSelfCheckChecksTheHost: bind's self-check checks the module for
// the host target as well as for the target it generated for, so a declaration
// naming a `target(web) type without carrying the target itself — which
// resolves on wasm32-web — fails bind rather than a consumer checking the
// module on any other target (#810; docs/web-apps.md §"Bind time self check").
func TestRunBindSelfCheckChecksTheHost(t *testing.T) {
	if dir := os.Getenv(selfCheckDirEnv); dir != "" {
		runBindSelfCheck(dir, "web")
		return
	}

	t.Run("a declaration missing its target fails", func(t *testing.T) {
		dir := writeSelfCheckProject(t, "")

		// It resolves on the bind target, so only the host check can reject it.
		cfg, files, err := discoverProject(dir, false)
		if err != nil || cfg == nil {
			t.Fatalf("discoverProject: cfg %v, err %v", cfg, err)
		}
		compileProjectFrontend(cfg.Dir, files, "wasm32-web")

		out, err := runSubprocess(t, "TestRunBindSelfCheckChecksTheHost", selfCheckDirEnv+"="+dir)
		if err == nil {
			t.Fatalf("the self-check passed a type naming a `target(web) type without the target (want it to fail on the host)\noutput:\n%s", out)
		}
		if !strings.Contains(out, "undefined type: Node") {
			t.Errorf("want an `undefined type: Node` error from the host check\noutput:\n%s", out)
		}
	})

	t.Run("a declaration carrying its target passes", func(t *testing.T) {
		runBindSelfCheck(writeSelfCheckProject(t, " `target(web)"), "web")
	})
}

// TestRunBindTypeNamingWrapperChecksOnHost drives bind end-to-end, self-check
// on, over a type naming a wrapper: a WebIDL dictionary naming an interface,
// directly and through another dictionary, and a WIT record and variant naming
// a resource. Each carries the module's target, so the module checks for both
// targets and bind returns; a type naming no wrapper carries none (#810). Under
// a keep list — how promise-language/web binds its spec set — the rule holds
// for what the list leaves: a dictionary whose only interface member names a
// dropped interface loses that member and carries no target.
func TestRunBindTypeNamingWrapperChecksOnHost(t *testing.T) {
	cases := []struct {
		name  string
		file  string
		src   string
		keep  string // keep list passed as -keep, when not empty
		run   func(args []string)
		lines []string // declaration lines the module must hold
	}{
		{
			name: "webidl",
			file: "dom.idl",
			src: `[Exposed=Window] interface Node {};
dictionary D { Node n; };
dictionary E { D d; long x; };
dictionary P { long x; };
`,
			run: runBindWebIdl,
			lines: []string{
				"type D `structural(protocol: false) `public `target(web) `doc(\"`D` (WebIDL dictionary)\") {",
				"type E `structural(protocol: false) `public `target(web) `doc(\"`E` (WebIDL dictionary)\") {",
				"type P `structural(protocol: false) `public `doc(\"`P` (WebIDL dictionary)\") {",
			},
		},
		{
			name: "webidl with a keep list",
			file: "dom.idl",
			src: `[Exposed=Window] interface Node { attribute DOMString text; };
[Exposed=Window] interface Other {};
dictionary D { Node n; Other o; };
dictionary E { Other o; long x; };
`,
			keep: "Node\n",
			run:  runBindWebIdl,
			lines: []string{
				"type D `structural(protocol: false) `public `target(web) `doc(\"`D` (WebIDL dictionary)\") {",
				"type E `structural(protocol: false) `public `doc(\"`E` (WebIDL dictionary)\") {",
			},
		},
		{
			name: "wit",
			file: "api.wit",
			src: `package example:things@0.1.0;

interface api {
    resource descriptor {
        size: func() -> u64;
    }

    record holder {
        d: descriptor,
        n: u32,
    }

    variant pick {
        one(descriptor),
        empty,
    }

    record plain {
        x: u32,
    }
}
`,
			run: runBindWit,
			lines: []string{
				"type Holder `structural(protocol: false) `public `target(wasi) {",
				"enum Pick `public `target(wasi) {",
				"type Plain `structural(protocol: false) `public {",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			srcPath := filepath.Join(dir, tc.file)
			if err := os.WriteFile(srcPath, []byte(tc.src), 0644); err != nil {
				t.Fatal(err)
			}
			outDir := filepath.Join(dir, "out")
			args := []string{"-name", "mod", "-o", outDir}
			if tc.keep != "" {
				keepPath := filepath.Join(dir, "mod.keep")
				if err := os.WriteFile(keepPath, []byte(tc.keep), 0644); err != nil {
					t.Fatal(err)
				}
				args = append(args, "-keep", keepPath)
			}

			silenceStdout(t, func() {
				tc.run(append(args, srcPath))
			})

			pr, err := os.ReadFile(filepath.Join(outDir, "mod.pr"))
			if err != nil {
				t.Fatalf("module not written: %v", err)
			}
			for _, line := range tc.lines {
				if !strings.Contains(string(pr), line) {
					t.Errorf("module is missing %q; got:\n%s", line, pr)
				}
			}
		})
	}
}
