package bindgen

import (
	"reflect"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/webidl"
)

// #18: `promise bind webidl -keep` generates only the interfaces and members a
// keep list names, and closes over what that removes so nothing kept names a
// dropped definition (docs/wasm-bindings.md, "Generating a subset").

// keepIR runs the path `promise bind webidl -keep` takes: parse, merge partials
// and mixins, apply the keep list, convert.
func keepIR(t *testing.T, src, list string) (*Module, []KeepDrop) {
	t.Helper()
	file, errs := webidl.Parse(src, "keep.webidl")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	if errs := webidl.Merge(file); len(errs) > 0 {
		t.Fatalf("merge errors: %v", errs)
	}
	keep, err := ParseKeepList("keep.txt", list)
	if err != nil {
		t.Fatalf("ParseKeepList: %v", err)
	}
	drops := ApplyKeepList(file, keep)
	return mustWebIdlToIR(t, file)[0], drops
}

// keepShape is what a test asserts about a converted module: each resource's
// methods by import name (unique per member, overload and accessor), each
// resource's parent, and each type's fields.
func keepShape(m *Module) (resources map[string][]string, parents map[string]string, types map[string][]string) {
	resources = make(map[string][]string)
	parents = make(map[string]string)
	for _, r := range m.Resources {
		var methods []string
		for _, f := range r.Methods {
			methods = append(methods, f.ImportName)
		}
		resources[r.Name] = methods
		if r.Parent != "" {
			parents[r.Name] = r.Parent
		}
	}
	types = make(map[string][]string)
	for _, ty := range m.Types {
		var fields []string
		for _, f := range ty.Fields {
			fields = append(fields, f.Name)
		}
		types[ty.Name] = fields
	}
	return resources, parents, types
}

func TestParseKeepList(t *testing.T) {
	keep, err := ParseKeepList("keep.txt", "Element\n  Element.tagName  \n\nEvent.constructor\r\nNodeList.@@iterator\nCSSPageDescriptors.margin-top\n")
	if err != nil {
		t.Fatalf("ParseKeepList: %v", err)
	}
	wantIfaces := map[string]bool{"Element": true}
	wantMembers := map[string]bool{
		"Element.tagName": true, "Event.constructor": true, "NodeList.@@iterator": true,
		"CSSPageDescriptors.margin-top": true,
	}
	if !reflect.DeepEqual(keep.interfaces, wantIfaces) {
		t.Errorf("interfaces = %v, want %v", keep.interfaces, wantIfaces)
	}
	if !reflect.DeepEqual(keep.members, wantMembers) {
		t.Errorf("members = %v, want %v", keep.members, wantMembers)
	}
}

// Every malformed line is reported, by line number, in one error — not only the
// first — and well-formed lines around them are not.
func TestParseKeepListReportsEveryMalformedLine(t *testing.T) {
	_, err := ParseKeepList("keep.txt", "Element\nElement.\n.tagName\na.b.c\na b\nNode.textContent\n")
	if err == nil {
		t.Fatal("ParseKeepList accepted malformed entries")
	}
	msg := err.Error()
	for _, want := range []string{`keep.txt:2: malformed keep entry "Element."`, `keep.txt:3: malformed keep entry ".tagName"`, `keep.txt:4: malformed keep entry "a.b.c"`, `keep.txt:5: malformed keep entry "a b"`} {
		if !strings.Contains(msg, want) {
			t.Errorf("error is missing %q\n%s", want, msg)
		}
	}
	for _, wellFormed := range []string{"keep.txt:1:", "keep.txt:6:"} {
		if strings.Contains(msg, wellFormed) {
			t.Errorf("error reports well-formed line %q\n%s", wellFormed, msg)
		}
	}
}

func TestApplyKeepList(t *testing.T) {
	cases := []struct {
		name      string
		src       string
		list      string
		resources map[string][]string
		parents   map[string]string // checked only when set
		types     map[string][]string
		drops     []KeepDrop
	}{
		{
			// Range is not listed, so it goes with its members — a listed member
			// does not keep its interface. Node is not listed either, but Element
			// inherits from it: it stays, with only its listed member, so
			// `Element is Node` resolves.
			name: "selection and ancestors",
			src: `interface Node {
				attribute DOMString textContent;
				Node appendChild(Node node);
			};
			interface Element : Node {
				readonly attribute DOMString tagName;
				undefined remove();
			};
			interface Range { undefined detach(); };`,
			list: "Element\nElement.tagName\nNode.textContent\nRange.detach\n",
			resources: map[string][]string{
				"Node":    {"Node.textContent.get", "Node.textContent.set"},
				"Element": {"Element.tagName.get"},
			},
			parents: map[string]string{"Element": "Node"},
			types:   map[string][]string{},
		},
		{
			// Constants, anonymous special operations and iterable declarations
			// have no entry of their own; they go with their interface.
			name: "unlisted members that go with their interface",
			src: `interface Node {};
			interface NodeList {
				const unsigned short MAX = 4;
				getter Node? (unsigned long index);
				readonly attribute unsigned long length;
				iterable<Node>;
			};`,
			list: "NodeList\nNode\n",
			resources: map[string][]string{
				"Node":     nil,
				"NodeList": {"NodeList.MAX", "NodeList.getter"},
			},
			types: map[string][]string{},
		},
		{
			// So do the members a maplike or setlike declaration declares.
			name: "maplike and setlike members go with their interface",
			src: `interface Counts { maplike<DOMString, long>; };
			interface Tags { readonly setlike<DOMString>; };`,
			list: "Counts\nTags\n",
			resources: map[string][]string{
				"Counts": {"Counts.size.get", "Counts.has", "Counts.get", "Counts.set", "Counts.delete", "Counts.clear"},
				"Tags":   {"Tags.size.get", "Tags.has"},
			},
			types: map[string][]string{},
		},
		{
			// A mutator the interface writes in place of the declaration's is a
			// written member, kept by its own entry: no declared delete or clear
			// stands in for the unlisted ones.
			name: "a written setlike mutator is listed",
			src: `interface FontFace {};
			interface FontFaceSet {
				setlike<FontFace>;
				FontFaceSet add(FontFace font);
				boolean delete(FontFace font);
				undefined clear();
			};`,
			list: "FontFace\nFontFaceSet\nFontFaceSet.add\n",
			resources: map[string][]string{
				"FontFace":    nil,
				"FontFaceSet": {"FontFaceSet.size.get", "FontFaceSet.has", "FontFaceSet.add"},
			},
			types: map[string][]string{},
		},
		{
			// Each member is its own unit: those naming the dropped value type
			// go, the rest stay.
			name: "maplike members naming a dropped definition",
			src: `interface Gone {};
			interface Counts { maplike<DOMString, Gone>; };`,
			list: "Counts\n",
			resources: map[string][]string{
				"Counts": {"Counts.size.get", "Counts.has", "Counts.delete", "Counts.clear"},
			},
			types: map[string][]string{},
			drops: []KeepDrop{
				{What: "Counts.get", Ref: "Gone"},
				{What: "Counts.set", Ref: "Gone"},
			},
		},
		{
			name: "constructor entry keeps every constructor",
			src: `interface ImageData {
				constructor(unsigned long sw, unsigned long sh);
				constructor(sequence<octet> data, unsigned long sw);
				readonly attribute unsigned long width;
			};
			interface Event { constructor(DOMString type); };`,
			list: "ImageData\nImageData.constructor\nEvent\n",
			resources: map[string][]string{
				"ImageData": {"ImageData.constructor", "ImageData.constructor.2"},
				"Event":     nil,
			},
			types: map[string][]string{},
		},
		{
			// An overload set is one entry, so the list can never keep part of
			// it: the ordinals are the ones the unfiltered IR assigns.
			name: "overload set kept whole",
			src: `interface TrustedHTML {};
			interface Element {
				undefined setAttribute(DOMString name, DOMString value);
				undefined setAttribute(DOMString name, TrustedHTML value);
				undefined remove();
			};`,
			list: "Element\nElement.setAttribute\nTrustedHTML\n",
			resources: map[string][]string{
				"TrustedHTML": nil,
				"Element":     {"Element.setAttribute", "Element.setAttribute.2"},
			},
			types: map[string][]string{},
		},
		{
			// A named legacycaller is the regular operation of its name, so the
			// list names it by that name, never by its keyword: listed, it is
			// kept with the rest of its overload set; unlisted, it is dropped.
			name: "named legacycaller listed by its name",
			src: `interface Element {};
			interface HTMLAllCollection {
				legacycaller Element? item(unsigned long index);
				Element? item(DOMString name);
			};
			interface Plugin {
				legacycaller Element? item(unsigned long index);
				readonly attribute DOMString name;
			};`,
			list: "Element\nHTMLAllCollection\nHTMLAllCollection.item\nPlugin\nPlugin.name\n",
			resources: map[string][]string{
				"Element":           nil,
				"HTMLAllCollection": {"HTMLAllCollection.item", "HTMLAllCollection.item.2"},
				"Plugin":            {"Plugin.name.get"},
			},
			types: map[string][]string{},
		},
		{
			// The list selects from the input: entries naming nothing in it —
			// absent interfaces, absent members, BCD keys that are not IDL
			// members — select nothing and are not errors.
			name: "entries naming nothing are ignored",
			src:  `interface Element { readonly attribute DOMString id; };`,
			list: "Element\nElement.nope\nMissing\nMissing.thing\nElement.click_event\nNodeList.@@iterator\n",
			resources: map[string][]string{
				"Element": nil,
			},
			types: map[string][]string{},
		},
		{
			// Entries are matched after the merge, so a mixin's member is listed
			// under the interface that includes it, not under the mixin.
			name: "mixin members are listed under the including interface",
			src: `interface mixin ParentNode {
				undefined append(DOMString nodes);
				readonly attribute unsigned long childElementCount;
			};
			interface Element {};
			Element includes ParentNode;`,
			list: "Element\nElement.append\nParentNode\nParentNode.childElementCount\n",
			resources: map[string][]string{
				"Element": {"Element.append"},
			},
			types: map[string][]string{},
		},
		{
			// Fancy is not listed. A member naming it — directly, nullable,
			// through a sequence, or as an anonymous getter's result — would not
			// compile, so it is dropped and reported. A union, a Promise and a
			// record lower to JsValue, so a name inside one is not a reference.
			name: "members naming a dropped interface",
			src: `interface Fancy {};
			interface Element {
				Fancy fancy();
				undefined many(sequence<Fancy> items);
				undefined either((Fancy or DOMString) value);
				Promise<Fancy> later();
				undefined table(record<DOMString, Fancy> entries);
				attribute Fancy? maybe;
				getter Fancy (unsigned long index);
				readonly attribute DOMString id;
			};`,
			list: "Element\nElement.fancy\nElement.many\nElement.either\nElement.later\nElement.table\nElement.maybe\nElement.id\n",
			resources: map[string][]string{
				"Element": {"Element.either", "Element.later", "Element.table", "Element.id.get"},
			},
			types: map[string][]string{},
			drops: []KeepDrop{
				{What: "Element.fancy", Ref: "Fancy"},
				{What: "Element.many", Ref: "Fancy"},
				{What: "Element.maybe", Ref: "Fancy"},
				{What: "Element.getter", Ref: "Fancy"},
			},
		},
		{
			// One overload naming a dropped interface drops the whole set, so the
			// surviving ordinals never depend on the list.
			name: "one dangling overload drops the set",
			src: `interface Fancy {};
			interface Element {
				undefined put(DOMString value);
				undefined put(Fancy value);
				undefined clear();
			};`,
			list: "Element\nElement.put\nElement.clear\n",
			resources: map[string][]string{
				"Element": {"Element.clear"},
			},
			types: map[string][]string{},
			drops: []KeepDrop{{What: "Element.put", Ref: "Fancy"}},
		},
		{
			// Constructors are one unit and their parameters are references:
			// one naming a dropped interface drops every constructor, reported
			// once however many of them name it.
			name: "a dangling constructor drops every constructor",
			src: `interface Fancy {};
			interface Widget {
				constructor();
				constructor(Fancy source);
				constructor(sequence<Fancy> sources);
				readonly attribute DOMString id;
			};`,
			list: "Widget\nWidget.constructor\nWidget.id\n",
			resources: map[string][]string{
				"Widget": {"Widget.id.get"},
			},
			types: map[string][]string{},
			drops: []KeepDrop{{What: "Widget.constructor", Ref: "Fancy"}},
		},
		{
			name: "a typedef of a dropped interface is dropped, and so is its user",
			src: `interface Fancy {};
			typedef Fancy FancyAlias;
			typedef DOMString Name;
			interface Element {
				undefined take(FancyAlias f);
				undefined named(Name n);
			};`,
			list: "Element\nElement.take\nElement.named\n",
			resources: map[string][]string{
				"Element": {"Element.named"},
			},
			types: map[string][]string{"Name": nil},
			drops: []KeepDrop{
				{What: "typedef FancyAlias", Ref: "Fancy"},
				{What: "Element.take", Ref: "FancyAlias"},
			},
		},
		{
			// An optional member naming a dropped definition is removed and the
			// dictionary stays. A required one cannot be removed without changing
			// what the host accepts, so the dictionary is dropped — and with it a
			// child dictionary and every member taking either.
			name: "dictionaries",
			src: `interface Fancy {};
			dictionary Opts { Fancy fancy; DOMString label; };
			dictionary Init { required Fancy fancy; };
			dictionary MoreInit : Init { DOMString extra; };
			interface Element {
				undefined configure(optional Opts opts = {});
				undefined start(Init init);
				undefined more(MoreInit init);
			};`,
			list: "Element\nElement.configure\nElement.start\nElement.more\n",
			resources: map[string][]string{
				"Element": {"Element.configure"},
			},
			types: map[string][]string{"Opts": {"label"}},
			drops: []KeepDrop{
				{What: "dictionary Init", Ref: "Fancy"},
				{What: "dictionary MoreInit", Ref: "Init"},
				{What: "Opts.fancy", Ref: "Fancy"},
				{What: "Element.start", Ref: "Init"},
				{What: "Element.more", Ref: "MoreInit"},
			},
		},
		{
			// A derived dictionary is flattened from its pruned parent, so an
			// optional member removed from the parent is gone from the child
			// too, and the removal is reported once, under the parent.
			name: "inherited optional member pruned once",
			src: `interface Fancy {};
			dictionary Base { Fancy fancy; DOMString label; };
			dictionary Derived : Base { DOMString extra; };
			interface Element {
				undefined go(optional Derived init = {});
			};`,
			list: "Element\nElement.go\n",
			resources: map[string][]string{
				"Element": {"Element.go"},
			},
			types: map[string][]string{
				"Base":    {"label"},
				"Derived": {"label", "extra"},
			},
			drops: []KeepDrop{
				{What: "Base.fancy", Ref: "Fancy"},
			},
		},
		{
			// InitAlias and Wrap are visited before Init is dropped, so only a
			// second pass sees that they name a dropped definition.
			name: "closure reaches a fixed point",
			src: `interface Fancy {};
			typedef Init InitAlias;
			dictionary Init { required Fancy fancy; };
			dictionary Wrap { required InitAlias init; };
			interface Element { undefined go(Wrap w); };`,
			list: "Element\nElement.go\n",
			resources: map[string][]string{
				"Element": nil,
			},
			types: map[string][]string{},
			drops: []KeepDrop{
				{What: "dictionary Init", Ref: "Fancy"},
				{What: "typedef InitAlias", Ref: "Init"},
				{What: "dictionary Wrap", Ref: "InitAlias"},
				{What: "Element.go", Ref: "Wrap"},
			},
		},
		{
			// #723: a name no bound IDL file defines counts as a dropped
			// definition, so a list never has to bind a spec only because what it
			// keeps names that spec's definitions.
			name: "a name absent from the input is dropped as a dropped definition is",
			src:  `interface Element { Missing find(); };`,
			list: "Element\nElement.find\n",
			resources: map[string][]string{
				"Element": nil,
			},
			types: map[string][]string{},
			drops: []KeepDrop{
				{What: "Element.find", Ref: "Missing", Unbound: true},
			},
		},
		{
			// #723's repro: the optional field is removed, the dictionary and
			// the member taking it stay.
			name: "an optional field naming an absent name is removed",
			src: `dictionary D { Missing m; long n; };
			[Exposed=Window] interface A { undefined use(optional D d = {}); };`,
			list: "A\nA.use\n",
			resources: map[string][]string{
				"A": {"A.use"},
			},
			types: map[string][]string{"D": {"n"}},
			drops: []KeepDrop{
				{What: "D.m", Ref: "Missing", Unbound: true},
			},
		},
		{
			// The dictionary is dropped for the absent name; the member taking it
			// is dropped for the dictionary, which the list's closure drops.
			name: "a required field naming an absent name drops its dictionary",
			src: `dictionary D { required Missing m; };
			interface A { undefined use(D d); undefined other(); };`,
			list: "A\nA.use\nA.other\n",
			resources: map[string][]string{
				"A": {"A.other"},
			},
			types: map[string][]string{},
			drops: []KeepDrop{
				{What: "dictionary D", Ref: "Missing", Unbound: true},
				{What: "A.use", Ref: "D"},
			},
		},
		{
			name: "a typedef naming an absent name is dropped with its uses",
			src: `typedef Missing M;
			interface A { attribute M m; undefined other(); };`,
			list: "A\nA.m\nA.other\n",
			resources: map[string][]string{
				"A": {"A.other"},
			},
			types: map[string][]string{},
			drops: []KeepDrop{
				{What: "typedef M", Ref: "Missing", Unbound: true},
				{What: "A.m", Ref: "M"},
			},
		},
		{
			name: "a parameter and a return type naming absent names",
			src: `interface A {
				undefined take(Missing m);
				Other give();
				readonly attribute long ok;
			};`,
			list: "A\nA.take\nA.give\nA.ok\n",
			resources: map[string][]string{
				"A": {"A.ok.get"},
			},
			types: map[string][]string{},
			drops: []KeepDrop{
				{What: "A.take", Ref: "Missing", Unbound: true},
				{What: "A.give", Ref: "Other", Unbound: true},
			},
		},
		{
			// A name inside a union is not a reference: the union lowers to
			// JsValue, which compiles whatever the union names.
			name: "a union naming an absent name is kept",
			src:  `interface A { attribute (Missing or DOMString) u; };`,
			list: "A\nA.u\n",
			resources: map[string][]string{
				"A": {"A.u.get", "A.u.set"},
			},
			types: map[string][]string{},
		},
		{
			// WindowProxy names Window, so with no Window bound the absent name
			// is Window — never WindowProxy, which no definition defines.
			name: "WindowProxy with no Window bound names Window",
			src: `interface A {
				readonly attribute WindowProxy? opener;
				readonly attribute long ok;
			};`,
			list: "A\nA.opener\nA.ok\n",
			resources: map[string][]string{
				"A": {"A.ok.get"},
			},
			types: map[string][]string{},
			drops: []KeepDrop{
				{What: "A.opener", Ref: "Window", Unbound: true},
			},
		},
		{
			// D keeps the unit m, so its ancestor P keeps P.m; P.m names an absent
			// definition, so the unit is dropped down the chain as for a dropped
			// one.
			name: "an absent name drops a unit across the chain",
			src: `interface P { Missing m(); };
			interface D : P { undefined m(long a); };`,
			list: "D\nD.m\n",
			resources: map[string][]string{
				"P": nil,
				"D": nil,
			},
			types: map[string][]string{},
			drops: []KeepDrop{
				{What: "P.m", Ref: "Missing", Unbound: true},
				{What: "D.m", Ref: "Missing", Unbound: true},
			},
		},
		{
			// #612: D's m shadows P's, so it is numbered after it. Listing D.m
			// keeps P.m too — the unit spans the chain — so D's ordinal is the
			// one the unfiltered IR assigns, whether or not P.m is listed.
			name: "a kept name is kept on the ancestors",
			src: `interface P {
				undefined m(DOMString a);
				readonly attribute DOMString other;
			};
			interface D : P { undefined m(long a); };`,
			list: "D\nD.m\n",
			resources: map[string][]string{
				"P": {"P.m"},
				"D": {"D.m.2"},
			},
			types: map[string][]string{},
		},
		{
			name: "a kept name reaches every ancestor",
			src: `interface P { undefined m(DOMString a); };
			interface C : P {};
			interface G : C { undefined m(long a); };`,
			list: "G\nG.m\n",
			resources: map[string][]string{
				"P": {"P.m"},
				"C": nil,
				"G": {"G.m.2"},
			},
			types: map[string][]string{},
		},
		{
			// The list selects from the input: D declares no m, so D.m names
			// nothing and keeps nothing up the chain.
			name: "an entry for an undeclared member keeps no ancestor's",
			src: `interface P { undefined m(DOMString a); };
			interface D : P {};`,
			list: "D\nD.m\n",
			resources: map[string][]string{
				"P": nil,
				"D": nil,
			},
			types: map[string][]string{},
		},
		{
			// P's m names a dropped definition, so the unit goes from P and from
			// D, which inherits it — otherwise D's m would lose the ordinal the
			// unfiltered IR gives it.
			name: "an ancestor's dangling name is dropped from its descendants",
			src: `interface Fancy {};
			interface P { undefined m(Fancy f); };
			interface D : P {
				undefined m(DOMString a);
				readonly attribute DOMString id;
			};`,
			list: "D\nD.m\nD.id\n",
			resources: map[string][]string{
				"P": nil,
				"D": {"D.id.get"},
			},
			types: map[string][]string{},
			drops: []KeepDrop{
				{What: "P.m", Ref: "Fancy"},
				{What: "D.m", Ref: "Fancy"},
			},
		},
		{
			// Nothing inherits from D, so its dangling m leaves P's alone.
			name: "a descendant's dangling name leaves the ancestor's",
			src: `interface Fancy {};
			interface P { undefined m(DOMString a); };
			interface D : P { undefined m(Fancy f); };`,
			list: "D\nD.m\n",
			resources: map[string][]string{
				"P": {"P.m"},
				"D": nil,
			},
			types: map[string][]string{},
			drops: []KeepDrop{{What: "D.m", Ref: "Fancy"}},
		},
		{
			// A constructor is a `global factory, never an inherited slot, so
			// its unit stops at its interface.
			name: "constructors are not kept up the chain",
			src: `interface P { constructor(); };
			interface D : P { constructor(DOMString s); };`,
			list: "D\nD.constructor\n",
			resources: map[string][]string{
				"P": nil,
				"D": {"D.constructor"},
			},
			types: map[string][]string{},
		},
		{
			// D is not kept, so its entry selects nothing — on D or up the chain.
			name: "an entry on a dropped interface keeps no ancestor's",
			src: `interface P { undefined m(DOMString a); };
			interface D : P { undefined m(long a); };`,
			list: "P\nD.m\n",
			resources: map[string][]string{
				"P": nil,
			},
			types: map[string][]string{},
		},
		{
			// The drop reaches every kept descendant, through C, which declares
			// no m and so reports none.
			name: "an ancestor's dangling name is dropped two levels down",
			src: `interface Fancy {};
			interface P { undefined m(Fancy f); };
			interface C : P { readonly attribute DOMString id; };
			interface G : C { undefined m(DOMString a); };`,
			list: "G\nG.m\nC.id\n",
			resources: map[string][]string{
				"P": nil,
				"C": {"C.id.get"},
				"G": nil,
			},
			types: map[string][]string{},
			drops: []KeepDrop{
				{What: "P.m", Ref: "Fancy"},
				{What: "G.m", Ref: "Fancy"},
			},
		},
		{
			// Constructors are never inherited, so P's dangling one leaves D's.
			name: "an ancestor's dangling constructor leaves the descendant's",
			src: `interface Fancy {};
			interface P { constructor(Fancy f); };
			interface D : P { constructor(DOMString s); };`,
			list: "D\nD.constructor\nP.constructor\n",
			resources: map[string][]string{
				"P": nil,
				"D": {"D.constructor"},
			},
			types: map[string][]string{},
			drops: []KeepDrop{{What: "P.constructor", Ref: "Fancy"}},
		},
		{
			// #685: the unit is the Promise name, so fooBar and foo_bar are one
			// overload set and one unit — listing either keeps both, and foo_bar
			// keeps the ordinal the unfiltered IR gives it.
			name: "differently spelled members of one Promise name are one unit",
			src: `interface I {
				undefined fooBar(long a);
				undefined foo_bar(DOMString s);
			};`,
			list: "I\nI.foo_bar\n",
			resources: map[string][]string{
				"I": {"I.fooBar", "I.foo_bar.2"},
			},
			types: map[string][]string{},
		},
		{
			// #719: an entry names a dashed member as the IDL spells it, and
			// margin-top lowers to marginTop's Promise name, so the entry keeps
			// both as one unit.
			name: "a dashed member and its camel-cased spelling are one unit",
			src: `interface I {
				attribute DOMString marginTop;
				attribute DOMString margin-top;
				readonly attribute long other;
			};`,
			list: "I\nI.margin-top\n",
			resources: map[string][]string{
				"I": {"I.marginTop.get", "I.marginTop.set", "I.margin-top.get", "I.margin-top.set"},
			},
			types: map[string][]string{},
		},
		{
			// #685: C's constant goes with C and is numbered after the reset it
			// inherits, so it keeps P's reset — the unit spans the chain whether
			// an entry or a member that goes with its interface keeps it.
			name: "a constant keeps the ancestor's member of its Promise name",
			src: `interface P { undefined reset(); };
			interface C : P { const long RESET = 1; };`,
			list: "C\n",
			resources: map[string][]string{
				"P": {"P.reset"},
				"C": {"C.RESET.2"},
			},
			types: map[string][]string{},
		},
		{
			// The anonymous getter goes with its interface, and so does the rest
			// of its unit: Getter, which numbers it second.
			name: "a member that goes with its interface keeps its unit",
			src: `interface E {};
			interface I {
				E Getter(DOMString s);
				getter E (unsigned long i);
			};`,
			list: "I\nE\n",
			resources: map[string][]string{
				"E": nil,
				"I": {"I.Getter", "I.getter.2"},
			},
			types: map[string][]string{},
		},
		{
			// P's fooBar names a dropped definition, so its unit goes from P and
			// from D, whose foo_bar is numbered after it.
			name: "a dangling unit is dropped down the chain under another spelling",
			src: `interface Fancy {};
			interface P { undefined fooBar(Fancy f); };
			interface D : P {
				undefined foo_bar(DOMString s);
				readonly attribute DOMString id;
			};`,
			list: "D\nD.foo_bar\nD.id\n",
			resources: map[string][]string{
				"P": nil,
				"D": {"D.id.get"},
			},
			types: map[string][]string{},
			drops: []KeepDrop{
				{What: "P.fooBar", Ref: "Fancy"},
				{What: "D.foo_bar", Ref: "Fancy"},
			},
		},
		{
			// The unit is dropped whole, and each IDL name it drops is reported:
			// the dangling one first, then the rest.
			name: "a dangling unit reports each name it drops",
			src: `interface Fancy {};
			interface I {
				undefined fooBar(DOMString s);
				undefined foo_bar(Fancy f);
				undefined other();
			};`,
			list: "I\nI.fooBar\nI.other\n",
			resources: map[string][]string{
				"I": {"I.other"},
			},
			types: map[string][]string{},
			drops: []KeepDrop{
				{What: "I.foo_bar", Ref: "Fancy"},
				{What: "I.fooBar", Ref: "Fancy"},
			},
		},
		{
			// A name whose own members name a dropped definition reports that
			// one, not the first its unit names.
			name: "each dangling name of a unit reports the definition it names",
			src: `interface F1 {};
			interface F2 {};
			interface I {
				undefined fooBar(F1 f);
				undefined foo_bar(F2 g);
				undefined other();
			};`,
			list: "I\nI.fooBar\nI.other\n",
			resources: map[string][]string{
				"I": {"I.other"},
			},
			types: map[string][]string{},
			drops: []KeepDrop{
				{What: "I.fooBar", Ref: "F1"},
				{What: "I.foo_bar", Ref: "F2"},
			},
		},
		{
			// A static is `global`, never an inherited slot, so C's constant is
			// `reset` with or without it: P's dangling static does not drop it,
			// and C keeping its unit does not keep the unlisted static.
			name: "an ancestor's static is not inherited",
			src: `interface Fancy {};
			interface P { static undefined reset(Fancy f); };
			interface C : P { const long RESET = 1; };`,
			list: "C\n",
			resources: map[string][]string{
				"P": nil,
				"C": {"C.RESET"},
			},
			types: map[string][]string{},
		},
		{
			name: "a kept name keeps no ancestor's static of it",
			src: `interface P {
				static undefined reset();
				static attribute long fooBar;
			};
			interface C : P {
				undefined reset();
				attribute long foo_bar;
			};`,
			list: "C\nC.reset\nC.foo_bar\n",
			resources: map[string][]string{
				"P": nil,
				"C": {"C.reset", "C.foo_bar.get", "C.foo_bar.set"},
			},
			types: map[string][]string{},
		},
		{
			// An operation whose name snakes to `constructor` joins the
			// constructors' overload set, so it is one unit with them.
			name: "constructors and an operation named Constructor are one unit",
			src: `interface Odd {
				constructor();
				undefined Constructor(long n);
			};`,
			list: "Odd\nOdd.Constructor\n",
			resources: map[string][]string{
				"Odd": {"Odd.constructor", "Odd.Constructor.2"},
			},
			types: map[string][]string{},
		},
		{
			// D's constructor is numbered after the `constructor` P's operation
			// is inherited as, so it keeps P's — an ancestor's unit holding an
			// instance member is inherited.
			name: "a constructor keeps an ancestor's operation of its Promise name",
			src: `interface P { undefined Constructor(long n); };
			interface D : P { constructor(); };`,
			list: "D\nD.constructor\n",
			resources: map[string][]string{
				"P": {"P.Constructor"},
				"D": {"D.constructor.2"},
			},
			types: map[string][]string{},
		},
		{
			name: "an ancestor's dangling operation drops a constructor of its Promise name",
			src: `interface Fancy {};
			interface P { undefined Constructor(Fancy f); };
			interface D : P { constructor(); };`,
			list: "D\nD.constructor\n",
			resources: map[string][]string{
				"P": nil,
				"D": nil,
			},
			types: map[string][]string{},
			drops: []KeepDrop{
				{What: "P.Constructor", Ref: "Fancy"},
				{What: "D.constructor", Ref: "Fancy"},
			},
		},
		{
			// The rule is by the ancestor's member kind, not by name: P's
			// Constructor is an instance operation, so D's entry reaches it.
			name: "an operation named Constructor keeps the ancestor's",
			src: `interface P { undefined Constructor(long n); };
			interface D : P { undefined Constructor(DOMString s); };`,
			list: "D\nD.Constructor\n",
			resources: map[string][]string{
				"P": {"P.Constructor"},
				"D": {"D.Constructor.2"},
			},
			types: map[string][]string{},
		},
		{
			// The iterable lowers to no member, so it shares no unit with the
			// operation whose Promise name its keyword spells: going with its
			// interface, it does not keep the unlisted Iterable.
			name: "a member that lowers to none is a unit apart",
			src: `interface I {
				iterable<long>;
				undefined Iterable();
			};`,
			list: "I\n",
			resources: map[string][]string{
				"I": nil,
			},
			types: map[string][]string{},
		},
		{
			// An entry names an IDL member, as compat data does: I.foo_bar names
			// nothing on an I declaring only fooBar, though both lower to foo_bar.
			name: "an entry names an IDL member, not its Promise name",
			src:  `interface I { undefined fooBar(); };`,
			list: "I\nI.foo_bar\n",
			resources: map[string][]string{
				"I": nil,
			},
			types: map[string][]string{},
		},
		{
			// The chain shape of #685 the other way: C's constant is numbered
			// after the reset it inherits, so when P's reset names a dropped
			// definition the constant goes too, though it names none itself.
			name: "a constant is dropped with an ancestor's dangling member of its Promise name",
			src: `interface Fancy {};
			interface P { undefined reset(Fancy f); };
			interface C : P { const long RESET = 1; };`,
			list: "C\n",
			resources: map[string][]string{
				"P": nil,
				"C": nil,
			},
			types: map[string][]string{},
			drops: []KeepDrop{
				{What: "P.reset", Ref: "Fancy"},
				{What: "C.RESET", Ref: "Fancy"},
			},
		},
		{
			// An instance attribute is an inherited slot as an operation is, so
			// D's foo_bar, numbered after P's fooBar, keeps it.
			name: "a kept name keeps an ancestor's attribute of it",
			src: `interface P { attribute long fooBar; };
			interface D : P { undefined foo_bar(DOMString s); };`,
			list: "D\nD.foo_bar\n",
			resources: map[string][]string{
				"P": {"P.fooBar.get", "P.fooBar.set"},
				"D": {"D.foo_bar.2"},
			},
			types: map[string][]string{},
		},
		{
			// Neither an iterable nor an anonymous legacycaller lowers to a
			// member, so neither is an inherited slot: P's, naming a dropped
			// definition, are dropped and leave D's.
			name: "an ancestor's dangling member that lowers to none leaves the descendant's",
			src: `interface Fancy {};
			interface P {
				iterable<Fancy>;
				legacycaller undefined (Fancy f);
			};
			interface D : P {
				iterable<long>;
				legacycaller undefined (long n);
			};`,
			list: "D\n",
			resources: map[string][]string{
				"P": nil,
				"D": nil,
			},
			types: map[string][]string{},
			drops: []KeepDrop{
				{What: "P.iterable", Ref: "Fancy"},
				{What: "P.legacycaller", Ref: "Fancy"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, drops := keepIR(t, tc.src, tc.list)
			resources, parents, types := keepShape(m)
			if !reflect.DeepEqual(resources, tc.resources) {
				t.Errorf("resources = %v\nwant        %v", resources, tc.resources)
			}
			if tc.parents != nil && !reflect.DeepEqual(parents, tc.parents) {
				t.Errorf("parents = %v, want %v", parents, tc.parents)
			}
			if !reflect.DeepEqual(types, tc.types) {
				t.Errorf("types = %v\nwant    %v", types, tc.types)
			}
			if !reflect.DeepEqual(drops, tc.drops) {
				t.Errorf("drops = %v\nwant    %v", drops, tc.drops)
			}
		})
	}
}
