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
	webidl.Merge(file)
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
	keep, err := ParseKeepList("keep.txt", "Element\n  Element.tagName  \n\nEvent.constructor\r\nNodeList.@@iterator\n")
	if err != nil {
		t.Fatalf("ParseKeepList: %v", err)
	}
	wantIfaces := map[string]bool{"Element": true}
	wantMembers := map[string]bool{"Element.tagName": true, "Event.constructor": true, "NodeList.@@iterator": true}
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
			// A definition absent from the input is not one the list dropped: the
			// member is kept, and fails to compile with `undefined type` as an
			// unbound parent does — the remedy is binding its spec, not hiding it.
			name: "a name absent from the input is not a dropped definition",
			src:  `interface Element { Missing find(); };`,
			list: "Element\nElement.find\n",
			resources: map[string][]string{
				"Element": {"Element.find"},
			},
			types: map[string][]string{},
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
