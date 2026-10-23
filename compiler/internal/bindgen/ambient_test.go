package bindgen

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/wit"
)

// documentRootRe matches the generated `document` root and captures the borrow
// factory it calls and the handle it wraps.
var documentRootRe = regexp.MustCompile("(?m)^get document Document `public `target\\(web\\) `doc\\(\"[^\"]+\"\\) \\{\n" +
	`    return Document\.(\w+)\((\d+)\);` + "\n}")

// TestCodegenAmbientRoots pins when the `document` root is emitted (#660): for a
// bound Document on the web target, as a borrowed wrapper over slot 2 built by
// the type's own non-owning factory — and never as a root naming a type the
// module does not define, nor on a target whose loader seeds no slots, nor for
// a WIT resource whose handles are not the glue's.
func TestCodegenAmbientRoots(t *testing.T) {
	cases := []struct {
		name    string
		idl     string
		target  string
		factory string // "" = no root
	}{
		{
			// A derived Document's own borrow hides Node's, so the root is a Document.
			name: "derived Document",
			idl: `interface Node { attribute DOMString textContent; };
			      interface Document : Node { readonly attribute DOMString title; };`,
			target:  "web",
			factory: "borrow",
		},
		{
			name:    "no Document",
			idl:     `interface Element { attribute DOMString id; };`,
			target:  "web",
			factory: "",
		},
		{
			name:    "wasi target",
			idl:     `interface Document { readonly attribute DOMString title; };`,
			target:  "wasi",
			factory: "",
		},
		{
			// An IDL member named borrow moves the factory; the root follows it.
			name:    "borrow member",
			idl:     `interface Document { undefined borrow(); };`,
			target:  "web",
			factory: "borrow_handle",
		},
		{
			// So does an inherited instance member named borrow (#684).
			name: "inherited borrow member",
			idl: `interface Node { undefined borrow(DOMString s); };
			      interface Document : Node { readonly attribute DOMString title; };`,
			target:  "web",
			factory: "borrow_handle",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := GeneratePromise(inheritanceModules(t, c.idl), c.target)
			if c.factory == "" {
				if strings.Contains(out, "get document ") {
					t.Fatalf("unexpected document root:\n%s", out)
				}
				return
			}
			roots := documentRootRe.FindAllStringSubmatch(out, -1)
			if len(roots) != 1 {
				t.Fatalf("want exactly one document root, got %d:\n%s", len(roots), out)
			}
			if got := roots[0][1]; got != c.factory {
				t.Errorf("root calls Document.%s, want Document.%s", got, c.factory)
			}
			if got := roots[0][2]; got != "2" {
				t.Errorf("root wraps handle %s, want 2 (the seeded document slot)", got)
			}
		})
	}

	// A WIT resource named document, bound for the web target, is not the page's
	// document: its handles come from the page's own imports, not the glue's ref
	// table, so wrapping slot 2 would name an unrelated object.
	t.Run("WIT resource", func(t *testing.T) {
		file, errs := wit.Parse(`interface dom { resource document { title: func() -> string; } }`, "dom.wit")
		if len(errs) > 0 {
			t.Fatalf("parse errors: %v", errs)
		}
		out := GeneratePromise(WitToIR(file), "web")
		if !strings.Contains(out, "type Document ") {
			t.Fatalf("the WIT resource did not bind as Document, so this case tests nothing:\n%s", out)
		}
		if strings.Contains(out, "get document ") {
			t.Fatalf("unexpected document root for a WIT resource:\n%s", out)
		}
	})
}

// seededGlobals returns the globals a generated loader seeds into the ref
// table, in seed order, read from its _ambientRefs literal. Both an entry's
// bare form (`globalThis,`) and its guarded form are accepted, so the check
// below still reads a hand-written list.
func seededGlobals(t *testing.T, js string) []string {
	t.Helper()
	const open = "const _ambientRefs = [\n"
	start := strings.Index(js, open)
	if start < 0 {
		t.Fatal("no _ambientRefs literal in the loader")
	}
	body := js[start+len(open):]
	end := strings.Index(body, "];")
	if end < 0 {
		t.Fatal("unterminated _ambientRefs literal")
	}
	guarded := regexp.MustCompile(`^typeof (\w+) !==`)
	var globals []string
	for _, line := range strings.Split(strings.TrimSpace(body[:end]), "\n") {
		entry := strings.TrimSuffix(strings.TrimSpace(line), ",")
		if m := guarded.FindStringSubmatch(entry); m != nil {
			entry = m[1]
		}
		globals = append(globals, entry)
	}
	return globals
}

// TestAmbientRootHandleMatchesSeedPosition is the one-source check of #660: the
// handle the generated `document` root wraps is the slot both loaders seed
// `document` into. It reads only the generated text — never ambientRefs — so it
// fails when either emitter stops rendering from that one definition and the
// order drifts on one side.
func TestAmbientRootHandleMatchesSeedPosition(t *testing.T) {
	modules := inheritanceModules(t, `interface Document { readonly attribute DOMString title; };`)
	root := documentRootRe.FindStringSubmatch(GeneratePromise(modules, "web"))
	if root == nil {
		t.Fatal("no document root generated for a bound Document")
	}
	handle, err := strconv.Atoi(root[2])
	if err != nil {
		t.Fatal(err)
	}
	loaders := map[string]string{
		"glue":      GenerateJSGlue(modules),
		"bootstrap": GenerateBootstrapJS("app.wasm"),
	}
	for name, js := range loaders {
		globals := seededGlobals(t, js)
		pos := -1
		for i, g := range globals {
			if g == "document" {
				pos = i
			}
		}
		if pos < 0 {
			t.Errorf("%s: document is not seeded (seed list %q)", name, globals)
			continue
		}
		// Slot 0 is the null sentinel, so seed position i is handle i+1.
		if handle != pos+1 {
			t.Errorf("%s: the root wraps handle %d, but document is seeded into slot %d (seed list %q)", name, handle, pos+1, globals)
		}
	}
}
