package webidl

import (
	"maps"
	"reflect"
	"slices"
	"testing"
)

// --- Lexer Tests ---

func TestLexerKeywords(t *testing.T) {
	src := `interface partial dictionary enum callback typedef mixin includes
		readonly attribute const static getter setter deleter constructor
		optional iterable async required`
	lex := NewLexer(src, "test.webidl")

	expected := []TokenKind{
		TokenInterface, TokenPartial, TokenDictionary, TokenEnum, TokenCallback,
		TokenTypedef, TokenMixin, TokenIncludes,
		TokenReadonly, TokenAttribute, TokenConst, TokenStatic,
		TokenGetter, TokenSetter, TokenDeleter, TokenConstructor,
		TokenOptional, TokenIterable, TokenAsync, TokenRequired,
		TokenEOF,
	}
	for _, exp := range expected {
		tok := lex.Next()
		if tok.Kind != exp {
			t.Errorf("expected %s, got %s (%q)", exp, tok.Kind, tok.Value)
		}
	}
}

func TestLexerBuiltinTypes(t *testing.T) {
	src := `void any object boolean byte octet short long unsigned
		unrestricted double float DOMString USVString ByteString
		sequence FrozenArray Promise record undefined`
	lex := NewLexer(src, "test.webidl")

	expected := []TokenKind{
		TokenVoid, TokenAny, TokenObject, TokenBoolean, TokenByte,
		TokenOctet, TokenShort, TokenLong, TokenUnsigned,
		TokenUnrestricted, TokenDouble, TokenFloatKw, TokenDOMString,
		TokenUSVString, TokenByteString,
		TokenSequence, TokenFrozenArray, TokenPromise, TokenRecord,
		TokenUndefined,
		TokenEOF,
	}
	for _, exp := range expected {
		tok := lex.Next()
		if tok.Kind != exp {
			t.Errorf("expected %s, got %s (%q)", exp, tok.Kind, tok.Value)
		}
	}
}

func TestLexerPunctuation(t *testing.T) {
	src := `{ } ( ) < > : ; , . = ? ...`
	lex := NewLexer(src, "test.webidl")

	expected := []TokenKind{
		TokenLBrace, TokenRBrace, TokenLParen, TokenRParen,
		TokenLAngle, TokenRAngle, TokenColon, TokenSemicolon,
		TokenComma, TokenDot, TokenEquals, TokenQuestion, TokenEllipsis,
		TokenEOF,
	}
	for _, exp := range expected {
		tok := lex.Next()
		if tok.Kind != exp {
			t.Errorf("expected %s, got %s (%q)", exp, tok.Kind, tok.Value)
		}
	}
}

func TestLexerStringLiteral(t *testing.T) {
	src := `"hello" "read-write" ""`
	lex := NewLexer(src, "test.webidl")

	cases := []struct {
		kind  TokenKind
		value string
	}{
		{TokenString, "hello"},
		{TokenString, "read-write"},
		{TokenString, ""},
		{TokenEOF, ""},
	}
	for _, c := range cases {
		tok := lex.Next()
		if tok.Kind != c.kind {
			t.Errorf("expected %s, got %s", c.kind, tok.Kind)
		}
		if tok.Kind == TokenString && tok.Value != c.value {
			t.Errorf("expected %q, got %q", c.value, tok.Value)
		}
	}
}

func TestLexerNumbers(t *testing.T) {
	src := `42 -1 0xFF 3.14`
	lex := NewLexer(src, "test.webidl")

	cases := []struct {
		kind  TokenKind
		value string
	}{
		{TokenInteger, "42"},
		{TokenInteger, "-1"},
		{TokenInteger, "0xFF"},
		{TokenFloat, "3.14"},
		{TokenEOF, ""},
	}
	for _, c := range cases {
		tok := lex.Next()
		if tok.Kind != c.kind {
			t.Errorf("expected %s, got %s (%q)", c.kind, tok.Kind, tok.Value)
		}
		if c.value != "" && tok.Value != c.value {
			t.Errorf("expected value %q, got %q", c.value, tok.Value)
		}
	}
}

func TestLexerIdentifier(t *testing.T) {
	src := `Element querySelector getElementById HTMLCanvasElement`
	lex := NewLexer(src, "test.webidl")

	expected := []string{"Element", "querySelector", "getElementById", "HTMLCanvasElement"}
	for _, exp := range expected {
		tok := lex.Next()
		if tok.Kind != TokenIdent {
			t.Errorf("expected identifier, got %s (%q)", tok.Kind, tok.Value)
		}
		if tok.Value != exp {
			t.Errorf("expected %q, got %q", exp, tok.Value)
		}
	}
}

func TestLexerComments(t *testing.T) {
	src := `interface // this is a comment
/* block comment */
Element`
	lex := NewLexer(src, "test.webidl")

	tok := lex.Next()
	if tok.Kind != TokenInterface {
		t.Errorf("expected interface, got %s", tok.Kind)
	}
	tok = lex.Next()
	if tok.Kind != TokenIdent || tok.Value != "Element" {
		t.Errorf("expected identifier 'Element', got %s %q", tok.Kind, tok.Value)
	}
}

func TestLexerPositions(t *testing.T) {
	src := "interface\nElement"
	lex := NewLexer(src, "test.webidl")

	tok := lex.Next()
	if tok.Pos.Line != 1 || tok.Pos.Column != 1 {
		t.Errorf("expected 1:1, got %d:%d", tok.Pos.Line, tok.Pos.Column)
	}
	tok = lex.Next()
	if tok.Pos.Line != 2 || tok.Pos.Column != 1 {
		t.Errorf("expected 2:1, got %d:%d", tok.Pos.Line, tok.Pos.Column)
	}
}

func TestLexerExtAttrs(t *testing.T) {
	src := `[Exposed=Window] interface Foo {};`
	lex := NewLexer(src, "test.webidl")

	tok := lex.Next()
	if tok.Kind != TokenIdent || tok.Value != "[Exposed=Window]" {
		t.Errorf("expected ext attr token, got %s %q", tok.Kind, tok.Value)
	}
}

// --- Parser Tests ---

func TestParseEmptyInterface(t *testing.T) {
	src := `interface Foo {};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(file.Interfaces) != 1 {
		t.Fatalf("expected 1 interface, got %d", len(file.Interfaces))
	}
	if file.Interfaces[0].Name != "Foo" {
		t.Errorf("expected interface name 'Foo', got %q", file.Interfaces[0].Name)
	}
}

func TestParseInterfaceInheritance(t *testing.T) {
	src := `interface Element : Node {};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if file.Interfaces[0].Parent != "Node" {
		t.Errorf("expected parent 'Node', got %q", file.Interfaces[0].Parent)
	}
}

func TestParseAttribute(t *testing.T) {
	src := `interface Foo {
		readonly attribute DOMString tagName;
		attribute unsigned long length;
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	iface := file.Interfaces[0]
	if len(iface.Members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(iface.Members))
	}

	attr1 := iface.Members[0].(*Attribute)
	if !attr1.Readonly {
		t.Error("expected readonly attribute")
	}
	if attr1.Name != "tagName" {
		t.Errorf("expected 'tagName', got %q", attr1.Name)
	}
	if attr1.Type.Builtin != "DOMString" {
		t.Errorf("expected DOMString type, got %q", attr1.Type.Builtin)
	}

	attr2 := iface.Members[1].(*Attribute)
	if attr2.Readonly {
		t.Error("expected non-readonly attribute")
	}
	if attr2.Type.Builtin != "unsigned long" {
		t.Errorf("expected 'unsigned long', got %q", attr2.Type.Builtin)
	}
}

func TestParseOperation(t *testing.T) {
	src := `interface Element {
		Element? querySelector(DOMString selectors);
		void setAttribute(DOMString name, DOMString value);
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	iface := file.Interfaces[0]
	if len(iface.Members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(iface.Members))
	}

	op1 := iface.Members[0].(*Operation)
	if op1.Name != "querySelector" {
		t.Errorf("expected 'querySelector', got %q", op1.Name)
	}
	if !op1.Return.Nullable {
		t.Error("expected nullable return type")
	}
	if len(op1.Params) != 1 {
		t.Fatalf("expected 1 param, got %d", len(op1.Params))
	}
	if op1.Params[0].Name != "selectors" {
		t.Errorf("expected param 'selectors', got %q", op1.Params[0].Name)
	}

	op2 := iface.Members[1].(*Operation)
	if op2.Name != "setAttribute" {
		t.Errorf("expected 'setAttribute', got %q", op2.Name)
	}
	if len(op2.Params) != 2 {
		t.Fatalf("expected 2 params, got %d", len(op2.Params))
	}
}

func TestParseConstructor(t *testing.T) {
	src := `interface Image {
		constructor(unsigned long width, unsigned long height);
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	iface := file.Interfaces[0]
	if len(iface.Members) != 1 {
		t.Fatalf("expected 1 member, got %d", len(iface.Members))
	}
	ctor := iface.Members[0].(*Constructor)
	if len(ctor.Params) != 2 {
		t.Fatalf("expected 2 params, got %d", len(ctor.Params))
	}
}

func TestParseStaticOperation(t *testing.T) {
	src := `interface Math {
		static double abs(double x);
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	op := file.Interfaces[0].Members[0].(*Operation)
	if !op.Static {
		t.Error("expected static operation")
	}
	if op.Name != "abs" {
		t.Errorf("expected 'abs', got %q", op.Name)
	}
}

func TestParseDictionary(t *testing.T) {
	src := `dictionary RequestInit {
		required DOMString method;
		DOMString body;
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(file.Dictionaries) != 1 {
		t.Fatalf("expected 1 dictionary, got %d", len(file.Dictionaries))
	}
	dict := file.Dictionaries[0]
	if dict.Name != "RequestInit" {
		t.Errorf("expected 'RequestInit', got %q", dict.Name)
	}
	if len(dict.Members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(dict.Members))
	}
	if !dict.Members[0].Required {
		t.Error("expected first member to be required")
	}
	if dict.Members[1].Required {
		t.Error("expected second member to be optional")
	}
}

func TestParseEnum(t *testing.T) {
	src := `enum RequestMode {
		"navigate",
		"same-origin",
		"no-cors",
		"cors"
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(file.Enums) != 1 {
		t.Fatalf("expected 1 enum, got %d", len(file.Enums))
	}
	e := file.Enums[0]
	if e.Name != "RequestMode" {
		t.Errorf("expected 'RequestMode', got %q", e.Name)
	}
	if len(e.Values) != 4 {
		t.Fatalf("expected 4 values, got %d", len(e.Values))
	}
	if e.Values[1] != "same-origin" {
		t.Errorf("expected 'same-origin', got %q", e.Values[1])
	}
}

func TestParseCallback(t *testing.T) {
	src := `callback EventHandler = undefined (Event event);`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(file.Callbacks) != 1 {
		t.Fatalf("expected 1 callback, got %d", len(file.Callbacks))
	}
	cb := file.Callbacks[0]
	if cb.Name != "EventHandler" {
		t.Errorf("expected 'EventHandler', got %q", cb.Name)
	}
	if cb.Return.Builtin != "undefined" {
		t.Errorf("expected undefined return, got %q", cb.Return.Builtin)
	}
}

func TestParseTypedef(t *testing.T) {
	src := `typedef unsigned long long DOMTimeStamp;`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(file.Typedefs) != 1 {
		t.Fatalf("expected 1 typedef, got %d", len(file.Typedefs))
	}
	td := file.Typedefs[0]
	if td.Name != "DOMTimeStamp" {
		t.Errorf("expected 'DOMTimeStamp', got %q", td.Name)
	}
	if td.Type.Builtin != "unsigned long long" {
		t.Errorf("expected 'unsigned long long', got %q", td.Type.Builtin)
	}
}

func TestParsePartialInterface(t *testing.T) {
	src := `interface Foo {
		readonly attribute DOMString name;
	};
	partial interface Foo {
		void doSomething();
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(file.Interfaces) != 1 {
		t.Fatalf("expected 1 interface, got %d", len(file.Interfaces))
	}
	if len(file.Partials) != 1 {
		t.Fatalf("expected 1 partial, got %d", len(file.Partials))
	}
	if file.Partials[0].Name != "Foo" {
		t.Errorf("expected partial name 'Foo', got %q", file.Partials[0].Name)
	}
}

func TestParseIncludes(t *testing.T) {
	src := `interface mixin Slottable {
		readonly attribute HTMLSlotElement? assignedSlot;
	};
	Element includes Slottable;`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(file.Includes) != 1 {
		t.Fatalf("expected 1 includes, got %d", len(file.Includes))
	}
	inc := file.Includes[0]
	if inc.Target != "Element" {
		t.Errorf("expected target 'Element', got %q", inc.Target)
	}
	if inc.Mixin != "Slottable" {
		t.Errorf("expected mixin 'Slottable', got %q", inc.Mixin)
	}
}

func TestParseMerge(t *testing.T) {
	src := `interface mixin NavigatorID {
		readonly attribute DOMString appName;
	};
	interface Navigator {};
	Navigator includes NavigatorID;`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	if errs := Merge(file); len(errs) > 0 {
		t.Fatalf("merge errors: %v", errs)
	}

	// Navigator should now have the mixin's member
	var nav *Interface
	for _, iface := range file.Interfaces {
		if iface.Name == "Navigator" {
			nav = iface
			break
		}
	}
	if nav == nil {
		t.Fatal("Navigator interface not found")
	}
	if len(nav.Members) != 1 {
		t.Fatalf("expected 1 member after merge, got %d", len(nav.Members))
	}
	attr := nav.Members[0].(*Attribute)
	if attr.Name != "appName" {
		t.Errorf("expected 'appName', got %q", attr.Name)
	}
}

func TestParseSequenceType(t *testing.T) {
	src := `interface Foo {
		sequence<DOMString> getNames();
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	op := file.Interfaces[0].Members[0].(*Operation)
	if op.Return.Kind != SequenceType {
		t.Errorf("expected SequenceType, got %d", op.Return.Kind)
	}
	if op.Return.Elem.Builtin != "DOMString" {
		t.Errorf("expected DOMString elem, got %q", op.Return.Elem.Builtin)
	}
}

func TestParsePromiseType(t *testing.T) {
	src := `interface Fetcher {
		Promise<Response> fetch(DOMString url);
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	op := file.Interfaces[0].Members[0].(*Operation)
	if op.Return.Kind != PromiseType {
		t.Errorf("expected PromiseType, got %d", op.Return.Kind)
	}
}

func TestParseNullableType(t *testing.T) {
	src := `interface Foo {
		readonly attribute Element? parent;
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	attr := file.Interfaces[0].Members[0].(*Attribute)
	if !attr.Type.Nullable {
		t.Error("expected nullable type")
	}
}

func TestParseOptionalParam(t *testing.T) {
	src := `interface Foo {
		void bar(DOMString a, optional long b);
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	op := file.Interfaces[0].Members[0].(*Operation)
	if op.Params[0].Optional {
		t.Error("expected first param non-optional")
	}
	if !op.Params[1].Optional {
		t.Error("expected second param optional")
	}
}

func TestParseVariadicParam(t *testing.T) {
	src := `interface Console {
		undefined log(any... data);
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	op := file.Interfaces[0].Members[0].(*Operation)
	if !op.Params[0].Variadic {
		t.Error("expected variadic param")
	}
}

// TestParseArgumentNameKeywords: every word of WebIDL's ArgumentNameKeyword
// production names an argument, as webref's `addEventListener(…, callback)` does.
func TestParseArgumentNameKeywords(t *testing.T) {
	for _, kw := range []string{
		"async", "attribute", "callback", "const", "constructor", "deleter",
		"dictionary", "enum", "getter", "includes", "inherit", "interface",
		"iterable", "maplike", "mixin", "namespace", "partial", "readonly",
		"required", "setlike", "setter", "static", "stringifier", "typedef",
		"unrestricted",
	} {
		t.Run(kw, func(t *testing.T) {
			src := "interface Foo { undefined f(long " + kw + ", DOMString after); };"
			file, errs := Parse(src, "test.webidl")
			if len(errs) > 0 {
				t.Fatalf("unexpected errors: %v", errs)
			}
			op := file.Interfaces[0].Members[0].(*Operation)
			if len(op.Params) != 2 {
				t.Fatalf("expected 2 params, got %d", len(op.Params))
			}
			if op.Params[0].Name != kw {
				t.Errorf("expected param %q, got %q", kw, op.Params[0].Name)
			}
			if op.Params[1].Name != "after" {
				t.Errorf("expected param 'after', got %q", op.Params[1].Name)
			}
		})
	}

	t.Run("optional defaulted", func(t *testing.T) {
		src := "interface Foo { undefined f(optional boolean callback = false); };"
		file, errs := Parse(src, "test.webidl")
		if len(errs) > 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		p := file.Interfaces[0].Members[0].(*Operation).Params[0]
		if p.Name != "callback" || !p.Optional || p.Default != "false" {
			t.Errorf("expected optional callback = false, got %+v", p)
		}
	})

	t.Run("variadic", func(t *testing.T) {
		src := "interface Foo { undefined f(any... interface); };"
		file, errs := Parse(src, "test.webidl")
		if len(errs) > 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		p := file.Interfaces[0].Members[0].(*Operation).Params[0]
		if p.Name != "interface" || !p.Variadic {
			t.Errorf("expected variadic interface, got %+v", p)
		}
	})
}

// TestParseKeywordOutsideArgumentNameErrors: an ArgumentNameKeyword is a name
// only in argument position; anywhere else it is still a keyword. And only those
// keywords name an argument: not any other keyword, and not a string literal
// that merely spells one.
func TestParseKeywordOutsideArgumentNameErrors(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"operation name", "interface Foo { undefined callback(); };", "got callback"},
		{"argument type", "interface Foo { undefined f(callback x); };", "expected type, got callback"},
		{"keyword outside the production", "interface Foo { undefined f(long optional); };", "expected ), got optional"},
		{"string literal spelling one", `interface Foo { undefined f(long "callback"); };`, "expected ), got string-literal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, errs := Parse(tt.src, "test.webidl")
			if len(errs) == 0 {
				t.Fatal("expected parse errors")
			}
			if !contains(errs[0].Error(), tt.want) {
				t.Errorf("expected first error to contain %q, got: %v", tt.want, errs)
			}
		})
	}
}

func TestParseConst(t *testing.T) {
	src := `interface Node {
		const unsigned short ELEMENT_NODE = 1;
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	c := file.Interfaces[0].Members[0].(*Const)
	if c.Name != "ELEMENT_NODE" {
		t.Errorf("expected 'ELEMENT_NODE', got %q", c.Name)
	}
	if c.Value != "1" {
		t.Errorf("expected value '1', got %q", c.Value)
	}
}

func TestParseGetter(t *testing.T) {
	src := `interface NodeList {
		getter Node? item(unsigned long index);
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	op := file.Interfaces[0].Members[0].(*Operation)
	if op.Special != "getter" {
		t.Errorf("expected special 'getter', got %q", op.Special)
	}
}

func TestParseIterable(t *testing.T) {
	src := `interface NodeList {
		iterable<Node>;
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	it := file.Interfaces[0].Members[0].(*Iterable)
	if it.ValueType.Name != "Node" {
		t.Errorf("expected Node value type, got %v", it.ValueType)
	}
}

func TestParseExtAttrs(t *testing.T) {
	src := `[Exposed=Window] interface Foo {};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	iface := file.Interfaces[0]
	if len(iface.ExtAttrs) != 1 {
		t.Fatalf("expected 1 ext attr, got %d", len(iface.ExtAttrs))
	}
	if iface.ExtAttrs[0].Name != "Exposed" {
		t.Errorf("expected 'Exposed', got %q", iface.ExtAttrs[0].Name)
	}
	if iface.ExtAttrs[0].Value != "Window" {
		t.Errorf("expected 'Window', got %q", iface.ExtAttrs[0].Value)
	}
}

func TestParseFullExample(t *testing.T) {
	src := `
	[Exposed=Window]
	interface Document : Node {
		Element? getElementById(DOMString elementId);
		Element createElement(DOMString localName);
		readonly attribute DOMString title;
	};

	dictionary EventInit {
		boolean bubbles = false;
		boolean cancelable = false;
	};

	enum ScrollBehavior {
		"auto",
		"instant",
		"smooth"
	};

	typedef unsigned long long DOMTimeStamp;

	callback EventListener = undefined (Event event);
	`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		for _, e := range errs {
			t.Errorf("parse error: %v", e)
		}
		t.FailNow()
	}

	if len(file.Interfaces) != 1 {
		t.Errorf("expected 1 interface, got %d", len(file.Interfaces))
	}
	if len(file.Dictionaries) != 1 {
		t.Errorf("expected 1 dictionary, got %d", len(file.Dictionaries))
	}
	if len(file.Enums) != 1 {
		t.Errorf("expected 1 enum, got %d", len(file.Enums))
	}
	if len(file.Typedefs) != 1 {
		t.Errorf("expected 1 typedef, got %d", len(file.Typedefs))
	}
	if len(file.Callbacks) != 1 {
		t.Errorf("expected 1 callback, got %d", len(file.Callbacks))
	}

	doc := file.Interfaces[0]
	if doc.Name != "Document" {
		t.Errorf("expected 'Document', got %q", doc.Name)
	}
	if doc.Parent != "Node" {
		t.Errorf("expected parent 'Node', got %q", doc.Parent)
	}
	if len(doc.Members) != 3 {
		t.Errorf("expected 3 members, got %d", len(doc.Members))
	}
}

func TestParseUnionType(t *testing.T) {
	src := `interface Foo {
		attribute (DOMString or long) value;
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	attr := file.Interfaces[0].Members[0].(*Attribute)
	if attr.Type.Kind != UnionType {
		t.Errorf("expected UnionType, got %d", attr.Type.Kind)
	}
	if len(attr.Type.Members) != 2 {
		t.Errorf("expected 2 union members, got %d", len(attr.Type.Members))
	}
}

func TestParseRecordType(t *testing.T) {
	src := `interface Foo {
		record<DOMString, long> getData();
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	op := file.Interfaces[0].Members[0].(*Operation)
	if op.Return.Kind != RecordType {
		t.Errorf("expected RecordType, got %d", op.Return.Kind)
	}
}

func TestParseDictionaryInheritance(t *testing.T) {
	src := `dictionary Base {
		DOMString name;
	};
	dictionary Derived : Base {
		long value;
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(file.Dictionaries) != 2 {
		t.Fatalf("expected 2 dictionaries, got %d", len(file.Dictionaries))
	}
	if file.Dictionaries[1].Parent != "Base" {
		t.Errorf("expected parent 'Base', got %q", file.Dictionaries[1].Parent)
	}
}

func TestParseStringifier(t *testing.T) {
	src := `interface URL {
		stringifier;
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	op := file.Interfaces[0].Members[0].(*Operation)
	if op.Name != "toString" {
		t.Errorf("expected 'toString', got %q", op.Name)
	}
}

func TestParseDefaultValue(t *testing.T) {
	src := `interface Foo {
		void bar(optional DOMString name = "default");
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	op := file.Interfaces[0].Members[0].(*Operation)
	if !op.Params[0].Optional {
		t.Error("expected optional param")
	}
}

func TestParseFrozenArrayType(t *testing.T) {
	src := `interface Foo {
		readonly attribute FrozenArray<DOMString> items;
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	attr := file.Interfaces[0].Members[0].(*Attribute)
	if attr.Type.Kind != FrozenArrayType {
		t.Errorf("expected FrozenArrayType, got %d", attr.Type.Kind)
	}
}

func TestParseStaticAttribute(t *testing.T) {
	src := `interface Performance {
		static readonly attribute DOMString name;
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	attr := file.Interfaces[0].Members[0].(*Attribute)
	if !attr.Static {
		t.Error("expected static attribute")
	}
	if !attr.Readonly {
		t.Error("expected readonly attribute")
	}
}

// --- Coverage gap tests ---

func TestTokenKindString(t *testing.T) {
	if s := TokenEOF.String(); s != "EOF" {
		t.Errorf("expected 'EOF', got %q", s)
	}
	if s := TokenInterface.String(); s != "interface" {
		t.Errorf("expected 'interface', got %q", s)
	}
	// Unknown token kind
	if s := TokenKind(9999).String(); s != "token(9999)" {
		t.Errorf("expected 'token(9999)', got %q", s)
	}
}

func TestParseStandaloneMixin(t *testing.T) {
	src := `mixin Slottable {
		readonly attribute DOMString slot;
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(file.Mixins) != 1 {
		t.Fatalf("expected 1 mixin, got %d", len(file.Mixins))
	}
	m := file.Mixins[0]
	if m.Name != "Slottable" {
		t.Errorf("expected 'Slottable', got %q", m.Name)
	}
	if len(m.Members) != 1 {
		t.Errorf("expected 1 member, got %d", len(m.Members))
	}
}

func TestParsePartialMixin(t *testing.T) {
	src := `mixin Base {
		readonly attribute DOMString name;
	};
	partial mixin Base {
		void doStuff();
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(file.Mixins) != 1 {
		t.Fatalf("expected 1 mixin, got %d", len(file.Mixins))
	}
	if len(file.Partials) != 1 {
		t.Fatalf("expected 1 partial, got %d", len(file.Partials))
	}
	partial := file.Partials[0]
	if partial.Name != "Base" {
		t.Errorf("expected 'Base', got %q", partial.Name)
	}
	if !partial.IsMixin {
		t.Error("expected IsMixin=true for partial mixin")
	}
}

func TestParsePartialDictionary(t *testing.T) {
	src := `dictionary Options {
		DOMString name;
	};
	partial dictionary Options {
		long timeout;
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(file.Dictionaries) != 1 {
		t.Fatalf("expected 1 dictionary, got %d", len(file.Dictionaries))
	}
	if len(file.PartialDicts) != 1 {
		t.Fatalf("expected 1 partial dict, got %d", len(file.PartialDicts))
	}
	pd := file.PartialDicts[0]
	if pd.Name != "Options" {
		t.Errorf("expected 'Options', got %q", pd.Name)
	}
	if len(pd.Members) != 1 {
		t.Fatalf("expected 1 member, got %d", len(pd.Members))
	}
	if pd.Members[0].Name != "timeout" {
		t.Errorf("expected member 'timeout', got %q", pd.Members[0].Name)
	}
	if pd.Members[0].Type.Builtin != "long" {
		t.Errorf("expected type 'long', got %q", pd.Members[0].Type.Builtin)
	}
}

func TestMergePartialDictionary(t *testing.T) {
	src := `dictionary Options {
		DOMString name;
	};
	partial dictionary Options {
		long timeout;
		required boolean verbose;
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if errs := Merge(file); len(errs) > 0 {
		t.Fatalf("merge errors: %v", errs)
	}
	dict := file.Dictionaries[0]
	if len(dict.Members) != 3 {
		t.Fatalf("expected 3 members after merge, got %d", len(dict.Members))
	}
	if dict.Members[1].Name != "timeout" {
		t.Errorf("expected second member 'timeout', got %q", dict.Members[1].Name)
	}
	if dict.Members[2].Name != "verbose" {
		t.Errorf("expected third member 'verbose', got %q", dict.Members[2].Name)
	}
	if !dict.Members[2].Required {
		t.Error("expected 'verbose' to be required")
	}
}

func TestParseMergePartialMixin(t *testing.T) {
	src := `mixin Mix {
		readonly attribute DOMString a;
	};
	partial mixin Mix {
		void b();
	};
	interface Target {};
	Target includes Mix;`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if errs := Merge(file); len(errs) > 0 {
		t.Fatalf("merge errors: %v", errs)
	}
	var target *Interface
	for _, iface := range file.Interfaces {
		if iface.Name == "Target" {
			target = iface
			break
		}
	}
	if target == nil {
		t.Fatal("Target interface not found")
	}
	// a + b = 2 members after partial merge + includes merge
	if len(target.Members) != 2 {
		t.Errorf("expected 2 members after merge, got %d", len(target.Members))
	}
}

func TestParseBuiltinTypesComprehensive(t *testing.T) {
	// Exercise all builtin type parsing branches in parseSingleType
	src := `interface Types {
		readonly attribute byte a;
		readonly attribute octet b;
		readonly attribute short c;
		readonly attribute unsigned short d;
		readonly attribute long long e;
		readonly attribute unsigned long long f;
		readonly attribute unrestricted float g;
		readonly attribute unrestricted double h;
		readonly attribute float i;
		readonly attribute any j;
		readonly attribute object k;
		readonly attribute symbol l;
		readonly attribute bigint m;
		readonly attribute void n;
		readonly attribute undefined o;
		readonly attribute ByteString p;
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	members := file.Interfaces[0].Members
	expected := []string{
		"byte", "octet", "short", "unsigned short",
		"long long", "unsigned long long",
		"unrestricted float", "unrestricted double",
		"float", "any", "object", "symbol", "bigint",
		"void", "undefined", "ByteString",
	}
	if len(members) != len(expected) {
		t.Fatalf("expected %d members, got %d", len(expected), len(members))
	}
	for i, exp := range expected {
		attr := members[i].(*Attribute)
		if attr.Type.Builtin != exp {
			t.Errorf("member[%d]: expected builtin %q, got %q", i, exp, attr.Type.Builtin)
		}
	}
}

func TestParseTypedArrayTypes(t *testing.T) {
	src := `interface Buffers {
		readonly attribute Float32Array a;
		readonly attribute Float64Array b;
		readonly attribute ArrayBuffer c;
		readonly attribute DataView d;
		readonly attribute Int8Array e;
		readonly attribute Int16Array f;
		readonly attribute Int32Array g;
		readonly attribute Uint8Array h;
		readonly attribute Uint16Array i;
		readonly attribute Uint32Array j;
		readonly attribute Uint8ClampedArray k;
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	members := file.Interfaces[0].Members
	expected := []string{
		"Float32Array", "Float64Array", "ArrayBuffer", "DataView",
		"Int8Array", "Int16Array", "Int32Array",
		"Uint8Array", "Uint16Array", "Uint32Array", "Uint8ClampedArray",
	}
	if len(members) != len(expected) {
		t.Fatalf("expected %d members, got %d", len(expected), len(members))
	}
	for i, exp := range expected {
		attr := members[i].(*Attribute)
		if attr.Type.Builtin != exp {
			t.Errorf("member[%d]: expected %q, got %q", i, exp, attr.Type.Builtin)
		}
	}
}

func TestParseObservableArrayType(t *testing.T) {
	src := `interface Foo {
		readonly attribute ObservableArray<long> items;
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	attr := file.Interfaces[0].Members[0].(*Attribute)
	if attr.Type.Kind != ObservableArrayType {
		t.Errorf("expected ObservableArrayType, got %d", attr.Type.Kind)
	}
}

func TestParseMemberSpecials(t *testing.T) {
	src := `interface Collection {
		setter void setItem(DOMString key, DOMString value);
		deleter void removeItem(DOMString key);
		stringifier attribute DOMString href;
		stringifier DOMString toString();
		async iterable<DOMString>;
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	members := file.Interfaces[0].Members
	if len(members) != 5 {
		t.Fatalf("expected 5 members, got %d", len(members))
	}
	setter := members[0].(*Operation)
	if setter.Special != "setter" {
		t.Errorf("expected special 'setter', got %q", setter.Special)
	}
	deleter := members[1].(*Operation)
	if deleter.Special != "deleter" {
		t.Errorf("expected special 'deleter', got %q", deleter.Special)
	}
	// A stringifier attribute binds like any attribute; Doc holds only its comment.
	strAttr := members[2].(*Attribute)
	if strAttr.Name != "href" || strAttr.Doc != "" {
		t.Errorf("expected attribute href with no doc, got %q with doc %q", strAttr.Name, strAttr.Doc)
	}
	strOp := members[3].(*Operation)
	if strOp.Special != "stringifier" {
		t.Errorf("expected special 'stringifier', got %q", strOp.Special)
	}
}

// TestParseLegacyCaller: `legacycaller` starts a special operation, named or
// anonymous, as webref's `HTMLAllCollection` uses it. The lexer leaves it an
// identifier, so anywhere else it still names something.
func TestParseLegacyCaller(t *testing.T) {
	t.Run("named, verbatim webref", func(t *testing.T) {
		src := `[Exposed=Window, LegacyUnenumerableNamedProperties]
		interface HTMLAllCollection {
			readonly attribute unsigned long length;
			getter Element (unsigned long index);
			getter (HTMLCollection or Element)? namedItem(DOMString name);
			legacycaller (HTMLCollection or Element)? item(optional DOMString nameOrIndex);
		};`
		file, errs := Parse(src, "test.webidl")
		if len(errs) > 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		members := file.Interfaces[0].Members
		if len(members) != 4 {
			t.Fatalf("expected 4 members, got %d", len(members))
		}
		op := members[3].(*Operation)
		if op.Name != "item" || op.Special != "legacycaller" {
			t.Errorf("expected legacycaller item, got %q with special %q", op.Name, op.Special)
		}
		if op.Return.Kind != UnionType || !op.Return.Nullable {
			t.Errorf("expected a nullable union return, got %+v", op.Return)
		}
		if len(op.Params) != 1 || op.Params[0].Name != "nameOrIndex" || !op.Params[0].Optional {
			t.Errorf("expected one optional param nameOrIndex, got %+v", op.Params)
		}
	})

	t.Run("anonymous", func(t *testing.T) {
		src := "interface Callable { legacycaller any (any... args); };"
		file, errs := Parse(src, "test.webidl")
		if len(errs) > 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		op := file.Interfaces[0].Members[0].(*Operation)
		if op.Name != "" || op.Special != "legacycaller" {
			t.Errorf("expected anonymous legacycaller, got %q with special %q", op.Name, op.Special)
		}
		if len(op.Params) != 1 || !op.Params[0].Variadic {
			t.Errorf("expected one variadic param, got %+v", op.Params)
		}
	})

	t.Run("an identifier elsewhere", func(t *testing.T) {
		src := "interface Foo { undefined f(long legacycaller, DOMString after); };"
		file, errs := Parse(src, "test.webidl")
		if len(errs) > 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		op := file.Interfaces[0].Members[0].(*Operation)
		if op.Special != "" {
			t.Errorf("expected a regular operation, got special %q", op.Special)
		}
		if len(op.Params) != 2 || op.Params[0].Name != "legacycaller" || op.Params[1].Name != "after" {
			t.Errorf("expected params legacycaller, after; got %+v", op.Params)
		}
	})

	// Unlike `stringifier;`, `legacycaller` has no shorthand: it needs a return type.
	t.Run("no return type", func(t *testing.T) {
		_, errs := Parse("interface Callable { legacycaller; };", "test.webidl")
		if len(errs) == 0 {
			t.Fatal("expected parse errors")
		}
		if want := "expected type, got ;"; !contains(errs[0].Error(), want) {
			t.Errorf("expected first error to contain %q, got: %v", want, errs)
		}
	})
}

func TestParseStaticNonReadonlyAttribute(t *testing.T) {
	src := `interface Foo {
		static attribute DOMString name;
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	attr := file.Interfaces[0].Members[0].(*Attribute)
	if !attr.Static {
		t.Error("expected static")
	}
	if attr.Readonly {
		t.Error("expected non-readonly")
	}
}

func TestParseIterableKeyValue(t *testing.T) {
	src := `interface URLSearchParams {
		iterable<USVString, USVString>;
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	it := file.Interfaces[0].Members[0].(*Iterable)
	if it.KeyType == nil {
		t.Fatal("expected key type")
	}
	if it.KeyType.Builtin != "USVString" {
		t.Errorf("expected USVString key, got %q", it.KeyType.Builtin)
	}
	if it.ValueType.Builtin != "USVString" {
		t.Errorf("expected USVString value, got %q", it.ValueType.Builtin)
	}
}

func TestParseErrorRecovery(t *testing.T) {
	// Invalid top-level token
	src := `@ interface Foo {};`
	_, errs := Parse(src, "test.webidl")
	if len(errs) == 0 {
		t.Error("expected parse errors")
	}
}

func TestParseErrorUnexpectedIdent(t *testing.T) {
	// Unexpected identifier at top level (not "includes")
	src := `Foo bar;`
	_, errs := Parse(src, "test.webidl")
	if len(errs) == 0 {
		t.Error("expected parse error for unexpected identifier")
	}
}

func TestParseErrorInPartial(t *testing.T) {
	// "partial" followed by unexpected token
	src := `partial enum Foo { "a" };`
	_, errs := Parse(src, "test.webidl")
	if len(errs) == 0 {
		t.Error("expected parse error for 'partial enum'")
	}
}

func TestParseErrorWithPosition(t *testing.T) {
	src := `@`
	_, errs := Parse(src, "test.webidl")
	if len(errs) == 0 {
		t.Fatal("expected errors")
	}
	errMsg := errs[0].Error()
	if errMsg == "" {
		t.Error("expected non-empty error message")
	}
	// Should contain filename
	if !contains(errMsg, "test.webidl") {
		t.Errorf("error should contain filename, got: %s", errMsg)
	}
}

func TestParseErrorNoFile(t *testing.T) {
	src := `@`
	_, errs := Parse(src, "")
	if len(errs) == 0 {
		t.Fatal("expected errors")
	}
	errMsg := errs[0].Error()
	// Should contain line:column without filename
	if contains(errMsg, ".webidl") {
		t.Errorf("error should not contain filename, got: %s", errMsg)
	}
}

func TestParsePartialInterfaceMixin(t *testing.T) {
	// "partial interface mixin" syntax
	src := `interface mixin Mix {
		readonly attribute DOMString a;
	};
	partial interface mixin Mix {
		void b();
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(file.Partials) != 1 {
		t.Fatalf("expected 1 partial, got %d", len(file.Partials))
	}
	if !file.Partials[0].IsMixin {
		t.Error("expected IsMixin=true for partial interface mixin")
	}
}

func TestLexerNumberExponent(t *testing.T) {
	src := `1e5 2.5E-3`
	lex := NewLexer(src, "test.webidl")

	tok := lex.Next()
	if tok.Kind != TokenFloat || tok.Value != "1e5" {
		t.Errorf("expected float '1e5', got %s %q", tok.Kind, tok.Value)
	}
	tok = lex.Next()
	if tok.Kind != TokenFloat || tok.Value != "2.5E-3" {
		t.Errorf("expected float '2.5E-3', got %s %q", tok.Kind, tok.Value)
	}
}

func TestParseExtAttrParenthesized(t *testing.T) {
	src := `[Constructor(DOMString name)] interface Foo {};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	iface := file.Interfaces[0]
	found := false
	for _, ea := range iface.ExtAttrs {
		if ea.Name == "Constructor" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected Constructor ext attr")
	}
}

func TestParseCallbackInterface(t *testing.T) {
	// "callback interface" is skipped and returns nil callback
	src := `callback interface EventListener {
		undefined handleEvent(Event event);
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(file.Callbacks) != 0 {
		t.Errorf("expected 0 callbacks (callback interface skipped), got %d", len(file.Callbacks))
	}
}

func TestParseDefaultValues(t *testing.T) {
	src := `interface Foo {
		void a(optional DOMString name = "hello");
		void b(optional long count = 42);
		void c(optional double ratio = 3.14);
		void d(optional boolean flag = true);
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	members := file.Interfaces[0].Members
	if len(members) != 4 {
		t.Fatalf("expected 4 members, got %d", len(members))
	}
	for _, m := range members {
		op := m.(*Operation)
		if !op.Params[0].Optional {
			t.Errorf("expected optional param in %s", op.Name)
		}
	}
}

func TestParseDictMemberDefault(t *testing.T) {
	src := `dictionary Init {
		boolean bubbles = false;
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	m := file.Dictionaries[0].Members[0]
	if m.Default != "false" {
		t.Errorf("expected default 'false', got %q", m.Default)
	}
}

// TestParseDictMemberExtAttr covers T0713: a dictionary member with a leading
// extended attribute. The ext-attr must be skipped so the type/name/default
// parse correctly (real IDL: element.idl CheckVisibilityOptions).
func TestParseDictMemberExtAttr(t *testing.T) {
	src := `dictionary CheckVisibilityOptions {
		[RuntimeEnabled=CheckVisibilityExtraProperties] boolean contentVisibilityAuto = false;
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	m := file.Dictionaries[0].Members[0]
	if m.Name != "contentVisibilityAuto" {
		t.Errorf("expected name 'contentVisibilityAuto', got %q", m.Name)
	}
	if m.Type == nil || m.Type.Builtin != "boolean" {
		t.Errorf("expected boolean type, got %+v", m.Type)
	}
	if m.Default != "false" {
		t.Errorf("expected default 'false', got %q", m.Default)
	}
}

// TestParseDictMemberEmptyDictDefault covers T0713: an empty-dictionary default
// value `{}` on a dictionary member (real IDL: element.idl SetHTMLUnsafeOptions).
func TestParseDictMemberEmptyDictDefault(t *testing.T) {
	src := `dictionary SetHTMLUnsafeOptions {
		(Sanitizer or SanitizerConfig) sanitizer = {};
		boolean runScripts = false;
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	members := file.Dictionaries[0].Members
	if len(members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(members))
	}
	if members[0].Default != "{}" {
		t.Errorf("expected default '{}', got %q", members[0].Default)
	}
	if members[1].Default != "false" {
		t.Errorf("expected default 'false', got %q", members[1].Default)
	}
}

// TestReadDefaultValueComplexFallback covers the lenient `default:` recovery
// branch in readDefaultValue (touched by T0713): a default value whose leading
// token is none of string/number/identifier/`{}` is consumed as a single token
// rather than aborting the parse. Documents the intentional "skip complex
// default values" leniency.
func TestReadDefaultValueComplexFallback(t *testing.T) {
	// '(' is not a recognized default-value start, so the fallback consumes it.
	src := `dictionary D {
		boolean flag = ( ;
	};`
	file, errs := Parse(src, "test.webidl")
	if len(file.Dictionaries) != 1 || len(file.Dictionaries[0].Members) != 1 {
		t.Fatalf("expected 1 dictionary with 1 member, got %+v (errs: %v)", file.Dictionaries, errs)
	}
	if got := file.Dictionaries[0].Members[0].Default; got != "(" {
		t.Errorf("expected fallback default '(', got %q", got)
	}
}

// TestParseParamEmptyDictDefault covers T0713: an empty-dictionary default `{}`
// on an optional operation parameter (real IDL: element.idl setHTML).
func TestParseParamEmptyDictDefault(t *testing.T) {
	src := `interface Element {
		void setHTML(DOMString html, optional SetHTMLOptions options = {});
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	op := file.Interfaces[0].Members[0].(*Operation)
	if len(op.Params) != 2 {
		t.Fatalf("expected 2 params, got %d", len(op.Params))
	}
	last := op.Params[1]
	if !last.Optional {
		t.Error("expected last param to be optional (has default)")
	}
	if last.Default != "{}" {
		t.Errorf("expected default '{}', got %q", last.Default)
	}
}

// TestParseUnionInlineExtAttr covers T0713: an inline extended attribute before
// a type inside a union, e.g. (TrustedHTML or [LegacyNullToEmptyString] DOMString).
// The ext-attr must be skipped so both union members parse.
func TestParseUnionInlineExtAttr(t *testing.T) {
	src := `interface Element {
		attribute (TrustedHTML or [LegacyNullToEmptyString] DOMString) innerHTML;
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	attr := file.Interfaces[0].Members[0].(*Attribute)
	if attr.Name != "innerHTML" {
		t.Errorf("expected name 'innerHTML', got %q", attr.Name)
	}
	if attr.Type.Kind != UnionType {
		t.Fatalf("expected UnionType, got %d", attr.Type.Kind)
	}
	if len(attr.Type.Members) != 2 {
		t.Fatalf("expected 2 union members, got %d", len(attr.Type.Members))
	}
	if attr.Type.Members[1].Name != "DOMString" && attr.Type.Members[1].Builtin != "DOMString" {
		t.Errorf("expected second union member to be DOMString, got %+v", attr.Type.Members[1])
	}
}

// TestParseEnumOnly covers the user's "possibly enum" suspicion in T0713: a
// standalone enum (with a quoted reserved-word value) parses to a Promise enum.
func TestParseEnumOnly(t *testing.T) {
	src := `enum SanitizerPresets { "default" };`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(file.Enums) != 1 {
		t.Fatalf("expected 1 enum, got %d", len(file.Enums))
	}
	e := file.Enums[0]
	if e.Name != "SanitizerPresets" {
		t.Errorf("expected 'SanitizerPresets', got %q", e.Name)
	}
	if len(e.Values) != 1 || e.Values[0] != "default" {
		t.Errorf("expected values [default], got %v", e.Values)
	}
}

func TestParseEnumInvalidToken(t *testing.T) {
	// Non-string token in enum body produces a parse error
	src := `enum Foo { 123 };`
	_, errs := Parse(src, "test.webidl")
	if len(errs) == 0 {
		t.Fatal("expected error for non-string token in enum")
	}
	found := false
	for _, e := range errs {
		if contains(e.Error(), "expected string literal in enum") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'expected string literal in enum' error, got: %v", errs)
	}
}

func TestReadDefaultFloat(t *testing.T) {
	// Float default value in dictionary member
	src := `dictionary Opts {
		double opacity = 1.0;
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	m := file.Dictionaries[0].Members[0]
	if m.Default != "1.0" {
		t.Errorf("expected default '1.0', got %q", m.Default)
	}
}

// TestMergeUnmatched pins #680: a partial whose interface, mixin or dictionary
// is not in the file, and each missing side of an includes statement, is
// reported at its position instead of being dropped, while every definition
// that matches is still applied and reported for nothing. members gives the
// member count each named interface or dictionary must have after the merge.
func TestMergeUnmatched(t *testing.T) {
	for _, tc := range []struct {
		name    string
		src     string
		want    []string
		members map[string]int
	}{
		{
			name: "partial interface",
			src:  "interface Base { readonly attribute DOMString name; };\npartial interface Unknown { undefined extra(); };",
			want: []string{
				"test.webidl:2:9: partial interface Unknown: Unknown is not an interface in the bound IDL files",
			},
			members: map[string]int{"Base": 1},
		},
		{
			name: "partial mixin",
			src:  "partial mixin Unknown { undefined extra(); };",
			want: []string{
				"test.webidl:1:9: partial mixin Unknown: Unknown is not a mixin in the bound IDL files",
			},
		},
		{
			name: "partial interface mixin",
			src:  "partial interface mixin Unknown { undefined extra(); };",
			want: []string{
				"test.webidl:1:9: partial mixin Unknown: Unknown is not a mixin in the bound IDL files",
			},
		},
		{
			// The standards' spelling, which the parser stores as an interface
			// marked _mixin; the partial comes first, as it does when the
			// extending spec is bound before the defining one.
			name:    "partial interface mixin on an interface mixin",
			src:     "partial interface mixin Mix { undefined b(); };\ninterface mixin Mix { undefined a(); };\ninterface Target {};\nTarget includes Mix;",
			members: map[string]int{"Target": 2},
		},
		{
			name: "partial dictionary",
			src:  "dictionary Base { DOMString name; };\npartial dictionary Unknown { long extra; };",
			want: []string{
				"test.webidl:2:9: partial dictionary Unknown: Unknown is not a dictionary in the bound IDL files",
			},
			members: map[string]int{"Base": 1},
		},
		{
			// The lookup is by kind: a dictionary of the name is not an interface.
			name: "partial interface naming a dictionary",
			src:  "dictionary Foo { DOMString name; };\npartial interface Foo { undefined extra(); };",
			want: []string{
				"test.webidl:2:9: partial interface Foo: Foo is not an interface in the bound IDL files",
			},
			members: map[string]int{"Foo": 1},
		},
		{
			name: "includes with no interface",
			src:  "interface mixin Mix { undefined extra(); };\nMissing includes Mix;",
			want: []string{
				"test.webidl:2:1: Missing includes Mix: Missing is not an interface in the bound IDL files",
			},
		},
		{
			name: "includes with no mixin",
			src:  "interface Target {};\nTarget includes Missing;",
			want: []string{
				"test.webidl:2:1: Target includes Missing: Missing is not a mixin in the bound IDL files",
			},
			members: map[string]int{"Target": 0},
		},
		{
			// The lookup is by kind on this side too: an interface is not a mixin.
			name: "includes naming an interface as its mixin",
			src:  "interface Base { undefined a(); };\ninterface Target {};\nTarget includes Base;",
			want: []string{
				"test.webidl:3:1: Target includes Base: Base is not a mixin in the bound IDL files",
			},
			members: map[string]int{"Target": 0},
		},
		{
			name: "includes with neither side",
			src:  "A includes B;",
			want: []string{
				"test.webidl:1:1: A includes B: A is not an interface in the bound IDL files",
				"test.webidl:1:1: A includes B: B is not a mixin in the bound IDL files",
			},
		},
		{
			name: "matched applied beside unmatched",
			src:  "interface Element { undefined click(); };\npartial interface Element { undefined focus(); };\npartial dictionary Opts { long extra; };\nElement includes ParentNode;",
			want: []string{
				"test.webidl:3:9: partial dictionary Opts: Opts is not a dictionary in the bound IDL files",
				"test.webidl:4:1: Element includes ParentNode: ParentNode is not a mixin in the bound IDL files",
			},
			members: map[string]int{"Element": 2},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, errs := Parse(tc.src, "test.webidl")
			if len(errs) > 0 {
				t.Fatalf("unexpected errors: %v", errs)
			}
			var got []string
			for _, e := range Merge(file) {
				got = append(got, e.Error())
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Merge errors\n%q\nwant\n%q", got, tc.want)
			}
			for name, want := range tc.members {
				count := -1
				for _, iface := range file.Interfaces {
					if iface.Name == name {
						count = len(iface.Members)
					}
				}
				for _, d := range file.Dictionaries {
					if d.Name == name {
						count = len(d.Members)
					}
				}
				if count != want {
					t.Errorf("%s has %d members after the merge, want %d", name, count, want)
				}
			}
		})
	}
}

// TestMergeRecordsRedeclared pins Interface.Redeclared (#682): Merge records on
// each interface the identifiers of its regular attributes and operations that
// an interface inheriting from it declares again on its prototype — as a
// regular attribute, regular operation or constant, whatever the signature —
// so the JS glue can call the declaring interface's own member. want lists,
// per interface, the identifiers recorded; an interface it omits records none.
func TestMergeRecordsRedeclared(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want map[string][]string
	}{
		{
			name: "attribute redeclared with another type",
			src: `interface SVGAnimatedString {};
			interface Element { attribute DOMString className; attribute DOMString id; };
			interface SVGElement : Element { readonly attribute SVGAnimatedString className; };`,
			want: map[string][]string{"Element": {"className"}},
		},
		{
			name: "named getter redeclared",
			src: `interface Element {};
			interface RadioNodeList {};
			interface HTMLCollection { getter Element? namedItem(DOMString name); };
			interface HTMLFormControlsCollection : HTMLCollection {
				getter (RadioNodeList or Element)? namedItem(DOMString name);
			};`,
			want: map[string][]string{"HTMLCollection": {"namedItem"}},
		},
		{
			// An exact redeclaration still replaces the member on the derived
			// prototype, so it counts like any other.
			name: "exact redeclaration",
			src: `interface RectReadOnly { readonly attribute double x; };
			interface Rect : RectReadOnly { attribute double x; };`,
			want: map[string][]string{"RectReadOnly": {"x"}},
		},
		{
			name: "attribute over operation and operation over attribute",
			src: `interface P { undefined a(); attribute long b; };
			interface C : P { attribute long a; undefined b(); };`,
			want: map[string][]string{"P": {"a", "b"}},
		},
		{
			// A constant lives on the interface prototype object too.
			name: "constant over attribute",
			src: `interface P { attribute long X; };
			interface C : P { const long X = 1; };`,
			want: map[string][]string{"P": {"X"}},
		},
		{
			name: "grandchild redeclares past a parent that does not",
			src: `interface A { undefined m(); undefined n(); };
			interface B : A { undefined o(); };
			interface C : B { undefined m(long x); };`,
			want: map[string][]string{"A": {"m"}},
		},
		{
			// Recorded after partials and includes are applied, on either side.
			name: "members from a mixin and from partials",
			src: `interface P { undefined m(); undefined n(); };
			partial interface P { undefined p(); };
			interface C : P {};
			interface mixin M { undefined m(); };
			C includes M;
			partial interface C { undefined p(); };`,
			want: map[string][]string{"P": {"m", "p"}},
		},
		{
			// None of these is a regular attribute, regular operation or
			// constant of the child. The iterable does put entries on C's
			// prototype, but WebIDL forbids an iterable declaration on an
			// interface that inherits a member named entries, so it is not
			// counted. An anonymous getter names no property on either side.
			name: "statics, constructor, anonymous getter and iterable on the child",
			src: `interface P {
				undefined s();
				attribute long t;
				undefined entries();
				getter long (unsigned long index);
			};
			interface C : P {
				static undefined s();
				static attribute long t;
				constructor();
				getter long (unsigned long index);
				iterable<long>;
			};`,
		},
		{
			// A static lives on the interface object, so the child's regular m
			// and a shadow nothing the parent's imports resolve.
			name: "statics on the parent",
			src: `interface P { static undefined m(); static attribute long a; };
			interface C : P { undefined m(); attribute long a; };`,
		},
		{
			name: "siblings and an unrelated interface",
			src: `interface P {};
			interface A : P { undefined m(); };
			interface B : P { undefined m(); };
			interface U { undefined m(); };`,
		},
		{
			name: "parent not in the file",
			src:  `interface C : Missing { undefined m(); };`,
		},
		{
			name: "inheritance cycle",
			src: `interface A : B { undefined m(); };
			interface B : A { undefined m(); };`,
			want: map[string][]string{"A": {"m"}, "B": {"m"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file, errs := Parse(tc.src, "test.webidl")
			if len(errs) > 0 {
				t.Fatalf("unexpected errors: %v", errs)
			}
			if errs := Merge(file); len(errs) > 0 {
				t.Fatalf("merge errors: %v", errs)
			}
			for _, iface := range file.Interfaces {
				got := slices.Sorted(maps.Keys(iface.Redeclared))
				if want := tc.want[iface.Name]; !slices.Equal(got, want) {
					t.Errorf("%s.Redeclared = %q, want %q", iface.Name, got, want)
				}
			}
		})
	}
}

// TestFileAppendCopiesEveryField walks File's fields by reflection rather than
// listing them, so a field added to File that Append does not copy fails here
// (#656: bind's hand-kept list dropped PartialDicts).
func TestFileAppendCopiesEveryField(t *testing.T) {
	dst, src := &File{}, &File{}
	dv, sv := reflect.ValueOf(dst).Elem(), reflect.ValueOf(src).Elem()
	typ := dv.Type()
	for i := 0; i < typ.NumField(); i++ {
		ft := typ.Field(i).Type
		if ft.Kind() != reflect.Slice || ft.Elem().Kind() != reflect.Pointer {
			t.Fatalf("File.%s is %s, not a slice of pointers: decide how Append combines it", typ.Field(i).Name, ft)
		}
		for _, v := range []reflect.Value{dv, sv} {
			v.Field(i).Set(reflect.Append(reflect.MakeSlice(ft, 0, 1), reflect.New(ft.Elem().Elem())))
		}
	}
	want := make([][2]any, typ.NumField())
	for i := range want {
		want[i] = [2]any{dv.Field(i).Index(0).Interface(), sv.Field(i).Index(0).Interface()}
	}

	dst.Append(src)

	for i := 0; i < typ.NumField(); i++ {
		name, got := typ.Field(i).Name, dv.Field(i)
		if got.Len() != 2 {
			t.Errorf("File.%s: got %d entries after Append, want 2 (dst's then src's)", name, got.Len())
			continue
		}
		if got.Index(0).Interface() != want[i][0] || got.Index(1).Interface() != want[i][1] {
			t.Errorf("File.%s: Append did not keep dst's entry first and add src's after it", name)
		}
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsImpl(s, substr))
}

func containsImpl(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// --- Doc comments (#659) ---

// memberDoc returns the Doc of a member kind that carries one.
func memberDoc(t *testing.T, m Member) string {
	t.Helper()
	switch m := m.(type) {
	case *Attribute:
		return m.Doc
	case *Operation:
		return m.Doc
	case *Constructor:
		return m.Doc
	case *Const:
		return m.Doc
	}
	t.Fatalf("member %T carries no doc", m)
	return ""
}

// The comments directly before a definition — before its extended attributes or
// between them and the declaration — become its Doc; a blank line, an
// intervening declaration, or a comment trailing code on its line detaches them.
func TestParseDefinitionDocComments(t *testing.T) {
	iface := func(f *File) string { return f.Interfaces[0].Doc }
	cases := []struct {
		name string
		src  string
		doc  func(*File) string
		want string
	}{
		{"block before ext attrs", "/** The root. */\n[Exposed=Window]\ninterface A {};", iface, "The root."},
		{"line between ext attrs and decl", "[Exposed=Window]\n// The root.\ninterface A {};", iface, "The root."},
		{"comment before ext attrs wins over one after them",
			"// Before.\n[Exposed=Window]\n// After.\ninterface A {};", iface, "Before."},
		{"decorated multi-line block", "/**\n * Line one.\n * Line two.\n */\ninterface A {};", iface, "Line one.\nLine two."},
		{"markdown bold is not decoration", "/**\n * **Bold** text.\n */\ninterface A {};", iface, "**Bold** text."},
		{"starred closer", "/** The root. **/\ninterface A {};", iface, "The root."},
		{"banner block", "/*****\n * The root.\n *****/\ninterface A {};", iface, "The root."},
		{"empty block", "/**/\ninterface A {};", iface, ""},
		{"consecutive line comments merge", "// One.\n// Two.\ninterface A {};", iface, "One.\nTwo."},
		{"empty line comment keeps the paragraph", "// One.\n//\n// Two.\ninterface A {};", iface, "One.\n\nTwo."},
		{"CRLF block", "/**\r\n * One.\r\n * Two.\r\n */\r\ninterface A {};", iface, "One.\nTwo."},
		{"CRLF line comments", "// One.\r\n// Two.\r\ninterface A {};", iface, "One.\nTwo."},
		{"interface mixin", "// M.\ninterface mixin M {};", iface, "M."},
		{"dictionary", "// D.\ndictionary D { long x; };", func(f *File) string { return f.Dictionaries[0].Doc }, "D."},
		{"enum", "/* E. */ enum E { \"a\" };", func(f *File) string { return f.Enums[0].Doc }, "E."},
		{"typedef", "// T.\ntypedef long T;", func(f *File) string { return f.Typedefs[0].Doc }, "T."},
		{"callback", "// C.\ncallback C = undefined (long x);", func(f *File) string { return f.Callbacks[0].Doc }, "C."},
		{"standalone mixin", "// M.\nmixin M {};", func(f *File) string { return f.Mixins[0].Doc }, "M."},

		{"no comment", "interface A {};", iface, ""},
		{"comment does not reach the next declaration", "// A.\ninterface A {};\ninterface B {};",
			func(f *File) string { return f.Interfaces[1].Doc }, ""},
		{"file header detached by a blank line",
			"// GENERATED CONTENT - DO NOT EDIT\n// Source: DOM Standard\n\n[Exposed=*]\ninterface Event {};", iface, ""},
		{"blank line keeps only the later run", "// first\n\n// second\ninterface A {};", iface, "second"},
		{"blank line holding indentation detaches", "// first\n  \t\ninterface A {};", iface, ""},
		{"comment trailing the previous declaration", "interface A {}; // about A\ninterface B {};",
			func(f *File) string { return f.Interfaces[1].Doc }, ""},
		{"trailing comment leaves the next line's comment attached", "interface A {}; // about A\n// B.\ninterface B {};",
			func(f *File) string { return f.Interfaces[1].Doc }, "B."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file, errs := Parse(tc.src, "test.webidl")
			if len(errs) > 0 {
				t.Fatalf("unexpected errors: %v", errs)
			}
			if got := tc.doc(file); got != tc.want {
				t.Errorf("doc = %q, want %q", got, tc.want)
			}
		})
	}
}

// Every member kind that becomes a declaration takes the comment before it.
func TestParseMemberDocComments(t *testing.T) {
	src := `interface A {
		// ro.
		readonly attribute long ro;
		/** rw. */
		attribute long rw;
		// st.
		static attribute long st;
		// op.
		undefined op();
		// sop.
		static undefined sop();
		// ov1.
		undefined ov(long x);
		// ov2.
		undefined ov(DOMString x);
		// get.
		getter long (unsigned long index);
		// ctor.
		constructor();
		// const.
		const long C = 1;
		// ce.
		[CEReactions] attribute long ce;
		[CEReactions]
		// ce2.
		attribute long ce2;
		// str.
		stringifier attribute DOMString href;
		// str2.
		stringifier;
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	want := []string{"ro.", "rw.", "st.", "op.", "sop.", "ov1.", "ov2.", "get.", "ctor.", "const.", "ce.", "ce2.", "str.", "str2."}
	members := file.Interfaces[0].Members
	if len(members) != len(want) {
		t.Fatalf("got %d members, want %d", len(members), len(want))
	}
	for i, w := range want {
		if got := memberDoc(t, members[i]); got != w {
			t.Errorf("member %d (%T) doc = %q, want %q", i, members[i], got, w)
		}
	}
}

// A comment that is not directly before a member documents nothing: one that
// trails a member's line, one inside a declaration, and one before `}`.
func TestParseMemberDocDetached(t *testing.T) {
	src := `interface A {
		attribute long x; // about x
		attribute /* inside */ long y;
		// before the brace
	};
	// before the next interface
	interface B { attribute long z; };`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	members := file.Interfaces[0].Members
	if len(members) != 2 {
		t.Fatalf("got %d members, want 2", len(members))
	}
	for i, m := range members {
		if got := memberDoc(t, m); got != "" {
			t.Errorf("member %d doc = %q, want none", i, got)
		}
	}
	if got := file.Interfaces[1].Doc; got != "before the next interface" {
		t.Errorf("B doc = %q, want the comment before it", got)
	}
	if got := memberDoc(t, file.Interfaces[1].Members[0]); got != "" {
		t.Errorf("B.z doc = %q, want none", got)
	}
}

// An unterminated block comment still runs to end of file, as before, without
// indexing past the end of the source.
func TestParseUnterminatedBlockComment(t *testing.T) {
	for _, src := range []string{
		"interface A {};\n/* never closed",
		"interface A {};\n/*",
		"interface A {};\n/*/",
		"interface A {};\n/**",
	} {
		file, errs := Parse(src, "test.webidl")
		if len(errs) > 0 {
			t.Errorf("%q: unexpected errors: %v", src, errs)
			continue
		}
		if len(file.Interfaces) != 1 || file.Interfaces[0].Doc != "" {
			t.Errorf("%q: want one interface with no doc, got %+v", src, file.Interfaces)
		}
	}
}
