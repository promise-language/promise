package bindgen

import (
	"strings"
	"testing"
)

// #766: a wrapper's Promise type is the type its IDL result declares — the
// Element createElement returns — so `as` can never reach the derived wrapper
// the host object is an instance of. Each derived interface instead gets a
// generated checked conversion, `C.from(Root wrapper) C?`, whose import asks
// the glue whether the host object is an instance of C's interface object, and
// answers with a new handle the converted wrapper owns, or with 0.

// conversionChainIDL is a chain of three derived interfaces under the root
// EventTarget, declared out of order, beside an unrelated root.
const conversionChainIDL = `interface HTMLInputElement : Element { attribute DOMString value; };
interface EventTarget {};
interface Node : EventTarget {};
interface Element : Node {};
interface Free {};`

// conversionLine builds the signature of the conversion to name, emitted as
// member and taking a wrapper of the chain root's type.
func conversionLine(name, member, root string) string {
	return member + "(" + root + " wrapper) " + name + "? `public `global `doc(\"" + conversionDoc + "\") {"
}

func TestCodegenConversionToDerivedInterface(t *testing.T) {
	out := GeneratePromise(overloadIR(t, conversionChainIDL), "web")

	// The whole conversion, wrapper and extern: the handle the import mints is
	// owned, and 0 — the object is not an instance — is none.
	assertContains(t, out, "    "+conversionLine("HTMLInputElement", "from", "EventTarget")+`
        handle := _html_input_element_from(wrapper._handle);
        if handle == 0 { return none; }
        return HTMLInputElement(_handle: handle, _owned: true);
    }
`)
	assertContains(t, out, `_html_input_element_from(i32 wrapper) i32
    `+"`"+`extern("html_input_element_from")
    `+"`"+`wasm_import("promise_env", "[cast]HTMLInputElement")
    `+"`"+`target(web);
`)

	// Every derived interface takes the chain's root, however deep it is.
	for _, name := range []string{"Node", "Element"} {
		assertContains(t, out, conversionLine(name, "from", "EventTarget"))
		assertContains(t, out, `wasm_import("promise_env", "[cast]`+name+`")`)
	}
	// A root has nothing to narrow from.
	for _, name := range []string{"EventTarget", "Free"} {
		assertNotContains(t, out, `"[cast]`+name+`"`)
		assertNotContains(t, out, "_"+toSnake(name)+"_from(")
	}
}

func TestGenerateJSGlueConversionToDerivedInterface(t *testing.T) {
	modules := overloadIR(t, conversionChainIDL)
	glue := GenerateJSGlue(modules)
	pr := GeneratePromise(modules, "web")

	assertContains(t, glue, `    "[cast]HTMLInputElement"($handle) {
      const $result = _refLoad($handle);
      return typeof HTMLInputElement === "function" && $result instanceof HTMLInputElement ? _refStore($result) : 0;
    },
`)
	// The glue implements exactly the conversion imports the module declares,
	// which are the derived interfaces' (TestCodegenConversionToDerivedInterface).
	for _, name := range []string{"Node", "Element", "HTMLInputElement", "EventTarget", "Free"} {
		key := `"` + conversionImportName(name) + `"`
		if inGlue, inPr := strings.Contains(glue, key+"("), strings.Contains(pr, key); inGlue != inPr {
			t.Errorf("%s: the glue has the import %v, the module declares it %v", key, inGlue, inPr)
		}
	}
}
