package buildrun

// Black-box tests for #785: a root project's sourced `use` reaches a module's
// module-level getters and functions whatever spelling it uses. The module
// object sema builds for a root import must carry the module's identity (what
// codegen keys IR prefixes on), not the spelling; before the fix only a
// spelling that happened to equal the identity (`"./m"` for a module directly
// under the project) did, and every other one panicked in codegen with
// `undefined module getter` / `undefined module function`.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
)

// identityModuleToml and identityModuleSrc are the module every case imports:
// one module-level getter and one plain function, the two codegen paths that
// resolve `m.<name>` through the module object's path.
const (
	identityModuleToml = "[module]\nname = \"m\"\nepoch = \"2026.0\"\n"
	identityModuleSrc  = "get value int `public `doc(\"The answer.\") {\n" +
		"    return 42;\n" +
		"}\n\n" +
		"add(int a, int b) int `public `doc(\"Adds two integers.\") {\n" +
		"    return a + b;\n" +
		"}\n"
	identityAppToml = "[module]\nname = \"app\"\nepoch = \"2026.0\"\n"
)

// identityConsumer is a root file that imports m by spelling and prints both
// members, so one run proves the getter and the function resolve.
func identityConsumer(spelling string) string {
	return "use m \"" + spelling + "\";\n\n" +
		"main() {\n" +
		"    print_line(\"{m.value} {m.add(1, 2)}\");\n" +
		"}\n"
}

func TestRootImportReachesModuleMembersWhateverItsSpelling(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping run integration test in short mode")
	}
	bin := clitest.Bin(t)

	cases := []struct {
		name  string
		app   string            // the project (or lone file) to run, relative to the case root
		files map[string]string // the whole layout, relative to the case root
	}{
		{
			// The item's host test: the project sits beside the module.
			name: "sibling",
			app:  "app",
			files: map[string]string{
				"m/promise.toml":   identityModuleToml,
				"m/m.pr":           identityModuleSrc,
				"app/promise.toml": identityAppToml,
				"app/main.pr":      identityConsumer("../m"),
			},
		},
		{
			// promise-language/web's check program: a project inside the
			// module's own tree, reaching the module above it.
			name: "inside the module's own tree",
			app:  "m/tests/check",
			files: map[string]string{
				"m/promise.toml":             identityModuleToml,
				"m/m.pr":                     identityModuleSrc,
				"m/tests/check/promise.toml": identityAppToml,
				"m/tests/check/main.pr":      identityConsumer("../.."),
			},
		},
		{
			// Inside the project, but not spelled as its identity `./lib/m`.
			name: "non-canonical in-project spelling",
			app:  "app",
			files: map[string]string{
				"app/promise.toml":       identityAppToml,
				"app/main.pr":            identityConsumer("./lib/../lib/m"),
				"app/lib/m/promise.toml": identityModuleToml,
				"app/lib/m/m.pr":         identityModuleSrc,
			},
		},
		{
			// A URL whose [replace] points at a local directory: the spelling
			// is the URL, the module's path is the replacement's directory.
			name: "replaced remote URL",
			app:  "app",
			files: map[string]string{
				"m/promise.toml": identityModuleToml,
				"m/m.pr":         identityModuleSrc,
				"app/promise.toml": identityAppToml +
					"\n[replace]\n\"github.com/acme/m\" = \"../m\"\n",
				"app/main.pr": identityConsumer("github.com/acme/m"),
			},
		},
		{
			// Imports are file-scoped: two files of one project may spell the
			// same module differently, and each spelling must reach it.
			name: "two root files, two spellings of one module",
			app:  "app",
			files: map[string]string{
				"m/promise.toml":   identityModuleToml,
				"m/m.pr":           identityModuleSrc,
				"app/promise.toml": identityAppToml,
				"app/main.pr": "use m \"../m\";\n\n" +
					"main() {\n" +
					"    print_line(\"{m.value} {sum()}\");\n" +
					"}\n",
				"app/other.pr": "use m \"./../m\";\n\n" +
					"sum() int {\n" +
					"    return m.add(1, 2);\n" +
					"}\n",
			},
		},
		{
			// The converse: one alias bound to two DIFFERENT modules, one per
			// file. The map is keyed by spelling, never by alias — keyed by
			// alias, both files would reach one module and print "42 42" or
			// "3 3".
			name: "one alias, two modules, one per root file",
			app:  "app",
			files: map[string]string{
				"m/promise.toml":   identityModuleToml,
				"m/m.pr":           identityModuleSrc,
				"m2/promise.toml":  "[module]\nname = \"m2\"\nepoch = \"2026.0\"\n",
				"m2/m2.pr":         "get value int `public {\n    return 3;\n}\n",
				"app/promise.toml": identityAppToml,
				"app/main.pr": "use m \"../m\";\n\n" +
					"main() {\n" +
					"    print_line(\"{m.value} {other()}\");\n" +
					"}\n",
				"app/other.pr": "use m \"../m2\";\n\n" +
					"other() int {\n" +
					"    return m.value;\n" +
					"}\n",
			},
		},
		{
			// No promise.toml anywhere above the file: the single-file
			// frontend, whose root is the file's own directory.
			name: "single file outside any project",
			app:  "solo/main.pr",
			files: map[string]string{
				"m/promise.toml": identityModuleToml,
				"m/m.pr":         identityModuleSrc,
				"solo/main.pr":   identityConsumer("../m"),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := clitest.TempDir(t)
			writeFiles(t, root, tc.files)

			res := clitest.Run(t, bin, nil, "run", filepath.Join(root, filepath.FromSlash(tc.app)))
			if res.ExitCode != 0 || strings.Contains(res.Combined(), "panic:") {
				t.Fatalf("run failed (want exit 0):%s", res.Detail())
			}
			if got := strings.TrimSpace(res.Stdout); got != "42 3" {
				t.Errorf("stdout = %q, want %q:%s", got, "42 3", res.Detail())
			}
		})
	}
}

// TestRootImportOfBoundWebModuleBuildsForWeb is the item's own repro: a
// `promise bind webidl` module beside the project, imported as `"../mini"`,
// whose `target(web) module getter `document` the program reads.
func TestRootImportOfBoundWebModuleBuildsForWeb(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind+build integration test in short mode")
	}
	bin := clitest.Bin(t)
	root := clitest.TempDir(t)

	writeFiles(t, root, map[string]string{
		"min.idl": `[Exposed=Window]
interface Node { attribute DOMString? textContent; };
[Exposed=Window]
interface Element : Node {};
[Exposed=Window]
interface Document : Node { Element createElement(DOMString localName, optional any options); };
`,
		"app/promise.toml": identityAppToml,
		"app/main.pr": `use web "../mini";

main() {
    doc := web.document;
    el := doc.create_element("p", none);
    el.text_content = "x";
}
`,
	})

	// bind writes mini/promise.toml itself, naming the module "web".
	bind := clitest.Run(t, bin, nil, "bind", "webidl", "-name", "web",
		"-o", filepath.Join(root, "mini"), filepath.Join(root, "min.idl"))
	if bind.ExitCode != 0 {
		t.Fatalf("bind webidl failed:%s", bind.Detail())
	}

	// The item's `-o out/main.wasm`; build writes into an existing directory
	// and does not create one, so the layout holds out/ as the item's did.
	app := filepath.Join(root, "app")
	wasm := filepath.Join(app, "out", "main.wasm")
	if err := os.MkdirAll(filepath.Dir(wasm), 0755); err != nil {
		t.Fatal(err)
	}
	res := clitest.Run(t, bin, nil, "build", "-target", "wasm32-web", "-o", wasm, app)
	if res.ExitCode != 0 || strings.Contains(res.Combined(), "panic:") {
		t.Fatalf("build -target wasm32-web failed (want exit 0):%s", res.Detail())
	}
	if _, err := os.Stat(wasm); err != nil {
		t.Errorf("expected %s: %v%s", wasm, err, res.Detail())
	}
}

// The two other root frontends take the same spelling → identity map: a
// module's own test suite (`promise test <module>`, whose root is the module
// under test) and `promise exec` (whose root is the working directory). Each
// panicked the same way before the fix.
func TestRootImportIdentityInModuleTestAndExec(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping test/exec integration test in short mode")
	}
	bin := clitest.Bin(t)

	t.Run("module test", func(t *testing.T) {
		t.Parallel()
		root := clitest.TempDir(t)
		writeFiles(t, root, map[string]string{
			"m/promise.toml":   identityModuleToml,
			"m/m.pr":           identityModuleSrc,
			"lib/promise.toml": "[module]\nname = \"lib\"\nepoch = \"2026.0\"\n",
			"lib/lib.pr": "use m \"../m\";\n\n" +
				"doubled() int `public {\n    return m.value * 2;\n}\n",
			"lib/lib_test.pr": "use m \"../m\";\n\n" +
				"test_reaches_sibling() `test {\n" +
				"    assert(m.value == 42, \"getter\");\n" +
				"    assert(m.add(1, 2) == 3, \"function\");\n" +
				"    assert(doubled() == 84, \"through the module's own source\");\n" +
				"}\n",
		})
		res := clitest.Run(t, bin, nil, "test", filepath.Join(root, "lib"))
		if res.ExitCode != 0 || strings.Contains(res.Combined(), "panic:") {
			t.Fatalf("test failed (want exit 0):%s", res.Detail())
		}
		if !strings.Contains(res.Combined(), "1 passed, 0 failed") {
			t.Errorf("want the one test to pass:%s", res.Detail())
		}
	})

	t.Run("exec", func(t *testing.T) {
		t.Parallel()
		root := clitest.TempDir(t)
		writeFiles(t, root, map[string]string{
			"m/promise.toml":   identityModuleToml,
			"m/m.pr":           identityModuleSrc,
			"app/promise.toml": identityAppToml,
		})
		// exec resolves a sourced `use` against its working directory.
		out := clitest.NewEnv(t).PromiseOK(t, filepath.Join(root, "app"),
			"exec", identityConsumer("../m"))
		if strings.Contains(out, "panic:") || !strings.Contains(out, "42 3") {
			t.Errorf("exec output = %q, want it to print %q", out, "42 3")
		}
	})
}
