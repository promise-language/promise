package bindgen

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// #168: webidl.Merge replaces a maplike or setlike declaration with the
// attribute and operations it declares, so each binds as an attribute or
// operation the IDL writes would: named, imported and lowered by the same
// rules (docs/wasm-bindings.md, "Supported subset"). get and set take the names
// every IDL get and set does.
func TestWebIdlToIRMaplikeSetlike(t *testing.T) {
	modules, err := WebIdlToIR(parseMerged(t, `interface Counts { maplike<DOMString, long>; };
interface Tags { setlike<DOMString>; };`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"Counts": {
			"Counts.size.get: get size() u32",
			"Counts.has: has(string key) bool",
			"Counts.get: _get(string key) i32?",
			"Counts.set: _set(string key, i32 value)",
			"Counts.delete: delete(string key) bool",
			"Counts.clear: clear()",
		},
		"Tags": {
			"Tags.size.get: get size() u32",
			"Tags.has: has(string value) bool",
			"Tags.add: add(string value)",
			"Tags.delete: delete(string value) bool",
			"Tags.clear: clear()",
		},
	}
	for _, r := range modules[0].Resources {
		var got []string
		for _, f := range r.Methods {
			got = append(got, methodSignature(f))
		}
		if !slices.Equal(got, want[r.Name]) {
			t.Errorf("%s methods =\n  %s\nwant\n  %s", r.Name, strings.Join(got, "\n  "), strings.Join(want[r.Name], "\n  "))
		}
	}
}

// methodSignature writes a method as its import and its Promise signature:
// "Counts.has: has(string key) bool", a getter prefixed "get".
func methodSignature(f Func) string {
	var params []string
	for _, p := range f.Params {
		params = append(params, promiseType(p.Type)+" "+p.Name)
	}
	accessor := ""
	if f.Accessor == AccessorGetter {
		accessor = "get "
	}
	sig := fmt.Sprintf("%s: %s%s(%s)", f.ImportName, accessor, f.Name, strings.Join(params, ", "))
	for _, r := range f.Results {
		sig += " " + promiseType(r)
	}
	return sig
}
