package bindgen

import (
	"reflect"
	"testing"
)

// #719: a WebIDL identifier may contain `-` and begin with one, as CSSOM's
// dashed attribute names do (`margin-top`). Its Promise name writes each `-` as
// `_` and drops a leading one; its import keeps the IDL spelling, and the glue
// reaches that member by it, with bracket access, since `obj.margin-top` is a
// subtraction.

// hyphenGlueFixture gives each kind of member access the glue spells a dashed
// name: Target's attribute, operation and static operation beside a
// camel-cased attribute of the same Promise name, and Base's attribute and
// operation, which Derived redeclares, so their imports call Base's own.
const hyphenGlueFixture = `interface Target {
  attribute long marginTop;
  attribute long margin-top;
  long do-it(long a-b);
  static long make-it();
};
interface Base {
  attribute long page-orientation;
  long run-it(long n);
};
interface Derived : Base {
  readonly attribute DOMString page-orientation;
  DOMString run-it(DOMString s);
};`

func TestWebIdlHyphenatedNamesLower(t *testing.T) {
	src := `interface I {
		attribute DOMString margin-top;
		readonly attribute DOMString -webkit-foo;
		undefined set-property(DOMString property-name);
	};
	dictionary PageInit {
		DOMString page-size;
		required long -webkit-x;
	};`
	modules := overloadIR(t, src)
	var got []overloadMethod
	var params []string
	for _, f := range overloadResource(t, modules, "I").Methods {
		got = append(got, describeMethod(f))
		for _, p := range f.Params {
			params = append(params, p.Name)
		}
	}
	want := []overloadMethod{
		{"margin_top", "I.margin-top.get", FuncMethod, ""},
		{"margin_top", "I.margin-top.set", FuncMethod, "string"},
		{"webkit_foo", "I.-webkit-foo.get", FuncMethod, ""},
		{"set_property", "I.set-property", FuncMethod, "string"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("I's members = %v\nwant           %v", got, want)
	}
	if wantParams := []string{"value", "property_name"}; !reflect.DeepEqual(params, wantParams) {
		t.Errorf("parameters = %q, want %q", params, wantParams)
	}
	// A dictionary member lowers by the same rule, at its own call site.
	var fields []string
	for _, ty := range modules[0].Types {
		if ty.Name == "PageInit" {
			for _, f := range ty.Fields {
				fields = append(fields, f.Name)
			}
		}
	}
	if wantFields := []string{"page_size", "webkit_x"}; !reflect.DeepEqual(fields, wantFields) {
		t.Errorf("PageInit fields = %q, want %q", fields, wantFields)
	}

	out := GeneratePromise(modules, "web")
	for _, s := range []string{
		"get margin_top string `public",
		"set margin_top(string value) `public",
		"get webkit_foo string `public",
		"set_property(this, string property_name) `public",
		"_i_margin_top(i32 handle) string",
		"_i_set_margin_top(i32 handle, string value)",
		"`extern(\"i_set_property\")",
		"\"I.margin-top.get\")",
		"\"I.-webkit-foo.get\")",
		"string? page_size;",
		"i32 webkit_x;",
	} {
		assertContains(t, out, s)
	}
}

func TestGenerateJSGlueHyphenatedMembers(t *testing.T) {
	glue := GenerateJSGlue(overloadIR(t, hyphenGlueFixture))
	for _, s := range []string{
		// A name that is a JS identifier keeps dot access.
		"\"Target.marginTop.get\"($handle) {\n      return _refLoad($handle).marginTop;\n",
		"\"Target.margin-top.get\"($handle) {\n      return _refLoad($handle)[\"margin-top\"];\n",
		"\"Target.margin-top.set\"($handle, value) {\n      _refLoad($handle)[\"margin-top\"] = value;\n",
		"\"Target.do-it\"($handle, a_b) {\n      return _refLoad($handle)[\"do-it\"](a_b);\n",
		"\"Target.make-it\"() {\n      return Target[\"make-it\"]();\n",
		"\"Base.page-orientation.get\"($handle) {\n      return Reflect.get(Base.prototype, \"page-orientation\", _refLoad($handle));\n",
		"\"Base.page-orientation.set\"($handle, value) {\n      Reflect.set(Base.prototype, \"page-orientation\", value, _refLoad($handle));\n",
		"\"Base.run-it\"($handle, n) {\n      return Reflect.apply(Base.prototype[\"run-it\"], _refLoad($handle), [n]);\n",
	} {
		assertContains(t, glue, s)
	}
}
