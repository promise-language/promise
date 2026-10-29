package webidl

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// --- Lexer Tests ---

func TestLexerKeywords(t *testing.T) {
	src := `interface partial dictionary enum callback typedef mixin namespace includes
		readonly attribute const static getter setter deleter constructor
		optional iterable async required`
	lex := NewLexer(src, "test.webidl")

	expected := []TokenKind{
		TokenInterface, TokenPartial, TokenDictionary, TokenEnum, TokenCallback,
		TokenTypedef, TokenMixin, TokenNamespace, TokenIncludes,
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
	src := `42 -1 0xFF 3.14 -1.5 -Infinity;`
	lex := NewLexer(src, "test.webidl")

	cases := []struct {
		kind  TokenKind
		value string
	}{
		{TokenInteger, "42"},
		{TokenInteger, "-1"},
		{TokenInteger, "0xFF"},
		{TokenFloat, "3.14"},
		{TokenFloat, "-1.5"},
		// -Infinity also matches the identifier token, but it is a float literal.
		{TokenFloat, "-Infinity"},
		{TokenSemicolon, ";"},
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

// TestLexerHyphenatedIdentifier pins #719: the identifier token is
// /[_-]?[A-Za-z][0-9A-Z_a-z-]*/, so a `-` continues an identifier and may lead
// one, as CSSOM's dashed attribute names (`margin-top`) need. A `-` before
// anything but a letter still starts a number, as it did before.
func TestLexerHyphenatedIdentifier(t *testing.T) {
	type tok struct {
		kind  TokenKind
		value string
	}
	cases := []struct {
		name string
		src  string
		want []tok
	}{
		{"inner hyphen", "margin-top", []tok{{TokenIdent, "margin-top"}}},
		{"leading hyphen", "-webkit-foo", []tok{{TokenIdent, "-webkit-foo"}}},
		{"leading underscore", "_underscored", []tok{{TokenIdent, "_underscored"}}},
		{"hyphen before a digit", "a-1", []tok{{TokenIdent, "a-1"}}},
		{"trailing hyphen", "foo-;", []tok{{TokenIdent, "foo-"}, {TokenSemicolon, ";"}}},
		// A keyword is looked up on the whole token: CSS's `object-fit` is not
		// the type keyword `object` followed by `-fit`.
		{"keyword before a hyphen", "object-fit", []tok{{TokenIdent, "object-fit"}}},
		{"spaced negative literal", "x = -1", []tok{{TokenIdent, "x"}, {TokenEquals, "="}, {TokenInteger, "-1"}}},
		{"hyphen before an underscore", "-_x", []tok{{TokenInteger, "-"}, {TokenIdent, "_x"}}},
		{"hyphen before a space", "- 1", []tok{{TokenInteger, "-"}, {TokenInteger, "1"}}},
		// The look past a `-` for a letter stops at the end of the input.
		{"hyphen at end of input", "x -", []tok{{TokenIdent, "x"}, {TokenInteger, "-"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lex := NewLexer(c.src, "test.webidl")
			var got []tok
			for next := lex.Next(); next.Kind != TokenEOF; next = lex.Next() {
				got = append(got, tok{next.Kind, next.Value})
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("tokens = %v, want %v", got, c.want)
			}
		})
	}

	// A leading hyphen is part of the token, so the token starts at it.
	lex := NewLexer("  -webkit-foo", "test.webidl")
	if tok := lex.Next(); tok.Pos.Line != 1 || tok.Pos.Column != 3 {
		t.Errorf("-webkit-foo at %d:%d, want 1:3", tok.Pos.Line, tok.Pos.Column)
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

// TestParseNamespace: a namespace holds regular operations, readonly attributes
// and constants, each with its doc comment and position, an operation keeping
// its leading extended attributes as it does on an interface. The namespace's
// own extended attributes are dropped, as a dictionary's are.
func TestParseNamespace(t *testing.T) {
	src := `// The console.
[Exposed=*]
namespace console {
  // Write a message.
  [LegacyUnforgeable] undefined log(any... data);
  readonly attribute DOMString label;
  const unsigned short MAX = 4;
};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(file.Namespaces) != 1 || len(file.Interfaces) != 0 || len(file.PartialNamespaces) != 0 {
		t.Fatalf("want one namespace and nothing else, got %d namespaces, %d interfaces, %d partial namespaces",
			len(file.Namespaces), len(file.Interfaces), len(file.PartialNamespaces))
	}
	ns := file.Namespaces[0]
	if ns.Name != "console" || ns.Doc != "The console." || ns.Pos != (Pos{File: "test.webidl", Line: 3, Column: 1}) {
		t.Errorf("namespace = %q, doc %q, at %+v; want console, %q, at 3:1", ns.Name, ns.Doc, ns.Pos, "The console.")
	}
	if len(ns.Members) != 3 {
		t.Fatalf("want 3 members, got %d", len(ns.Members))
	}
	op, ok := ns.Members[0].(*Operation)
	if !ok || op.Name != "log" || op.Static || op.Doc != "Write a message." || op.Pos.Line != 5 {
		t.Errorf("member 0 = %+v, want the operation log, not static, documented, on line 5", ns.Members[0])
	} else if len(op.ExtAttrs) != 1 || op.ExtAttrs[0].Name != "LegacyUnforgeable" {
		t.Errorf("log's extended attributes = %+v, want [LegacyUnforgeable]", op.ExtAttrs)
	}
	if a, ok := ns.Members[1].(*Attribute); !ok || a.Name != "label" || !a.Readonly || a.Static || a.Pos.Line != 6 {
		t.Errorf("member 1 = %+v, want the readonly attribute label on line 6", ns.Members[1])
	}
	if c, ok := ns.Members[2].(*Const); !ok || c.Name != "MAX" || c.Value != "4" || c.Pos.Line != 7 {
		t.Errorf("member 2 = %+v, want the constant MAX = 4 on line 7", ns.Members[2])
	}
}

func TestParsePartialNamespace(t *testing.T) {
	src := `partial namespace CSS {
  double px(double value);
};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(file.Namespaces) != 0 || len(file.PartialNamespaces) != 1 {
		t.Fatalf("want one partial namespace and no namespace, got %d partial, %d namespaces", len(file.PartialNamespaces), len(file.Namespaces))
	}
	pn := file.PartialNamespaces[0]
	if pn.Name != "CSS" || pn.Pos != (Pos{File: "test.webidl", Line: 1, Column: 9}) {
		t.Errorf("partial namespace = %q at %+v, want CSS at 1:9", pn.Name, pn.Pos)
	}
	if len(pn.Members) != 1 || pn.Members[0].(*Operation).Name != "px" {
		t.Errorf("members = %+v, want the operation px", pn.Members)
	}
}

// TestParseNamespaceMemberErrors: a namespace member is a regular operation, a
// readonly attribute or a constant. Any other member — in a namespace or a
// partial one — is reported where it starts and dropped, and the members after
// it still parse.
func TestParseNamespaceMemberErrors(t *testing.T) {
	for _, tc := range []struct {
		name, member string
	}{
		{"writable attribute", "attribute DOMString x;"},
		{"static operation", "static undefined f();"},
		{"static attribute", "static readonly attribute long x;"},
		{"constructor", "constructor();"},
		{"anonymous getter", "getter DOMString (unsigned long index);"},
		{"named getter", "getter DOMString item(unsigned long index);"},
		{"anonymous operation", "undefined (long x);"},
		{"iterable", "iterable<long>;"},
		{"maplike", "maplike<DOMString, long>;"},
		{"readonly setlike", "readonly setlike<long>;"},
		{"with extended attributes", "[SecureContext] attribute DOMString x;"},
		// `stringifier;` parses as the regular operation toString() and a
		// stringifier attribute as a plain one, so only the keyword marks them.
		{"stringifier", "stringifier;"},
		{"stringifier attribute", "stringifier readonly attribute DOMString s;"},
		{"stringifier with extended attributes", "[SecureContext] stringifier;"},
	} {
		for _, partial := range []string{"", "partial "} {
			t.Run(partial+tc.name, func(t *testing.T) {
				src := partial + "namespace N {\n  undefined before();\n  " + tc.member + "\n  undefined after();\n};"
				file, errs := Parse(src, "test.webidl")
				want := "test.webidl:3:3: namespace N: a namespace member is a regular operation, a readonly attribute or a constant"
				if len(errs) != 1 || errs[0].Error() != want {
					t.Fatalf("errors = %v, want exactly %q", errs, want)
				}
				nss := file.Namespaces
				if partial != "" {
					nss = file.PartialNamespaces
				}
				if len(nss) != 1 {
					t.Fatalf("want one namespace, got %d", len(nss))
				}
				var names []string
				for _, m := range nss[0].Members {
					names = append(names, m.(*Operation).Name)
				}
				if !reflect.DeepEqual(names, []string{"before", "after"}) {
					t.Errorf("members = %q, want [before after]: the bad member dropped, the next one kept", names)
				}
			})
		}
	}
}

// TestMergePartialNamespace: Merge appends a partial namespace's members to its
// namespace in input order, whether the partial comes before the namespace or
// after it, as the files binding the two may be ordered either way.
func TestMergePartialNamespace(t *testing.T) {
	src := `partial namespace CSS { double a(); };
namespace CSS { DOMString escape(DOMString ident); };
partial namespace CSS { double b(); };`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if errs := Merge(file); len(errs) > 0 {
		t.Fatalf("merge errors: %v", errs)
	}
	var names []string
	for _, m := range file.Namespaces[0].Members {
		names = append(names, m.(*Operation).Name)
	}
	if !reflect.DeepEqual(names, []string{"escape", "a", "b"}) {
		t.Errorf("CSS members after the merge = %q, want [escape a b]", names)
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

// #171: an inherit attribute (DOMPoint's and DOMRect's shape) parses as the
// read-write attribute it declares, keeping its extended attributes, and the
// members after it still parse. WebIDL's InheritAttribute is `inherit
// AttributeRest`, so `inherit` before anything but `attribute` is an error.
func TestParseInheritAttribute(t *testing.T) {
	src := `interface DOMPoint : DOMPointReadOnly {
		[Replaceable] inherit attribute unrestricted double x;
		inherit attribute unrestricted double y;
		undefined reset();
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	members := file.Interfaces[0].Members
	if len(members) != 3 {
		t.Fatalf("expected 3 members, got %d", len(members))
	}
	for i, name := range []string{"x", "y"} {
		attr, ok := members[i].(*Attribute)
		if !ok {
			t.Fatalf("member %d: expected *Attribute, got %T", i, members[i])
		}
		if attr.Name != name {
			t.Errorf("member %d: expected name %q, got %q", i, name, attr.Name)
		}
		if attr.Type.Builtin != "unrestricted double" {
			t.Errorf("member %d: expected 'unrestricted double', got %q", i, attr.Type.Builtin)
		}
		if attr.Readonly || attr.Static {
			t.Errorf("member %d: expected a read-write instance attribute, got readonly=%v static=%v", i, attr.Readonly, attr.Static)
		}
	}
	if !hasExtAttr(members[0].(*Attribute).ExtAttrs, "Replaceable") {
		t.Error("expected [Replaceable] kept on x")
	}
	if op, ok := members[2].(*Operation); !ok || op.Name != "reset" {
		t.Errorf("expected operation reset after the inherit attributes, got %#v", members[2])
	}

	errCases := []struct {
		name string
		src  string
		want string
	}{
		{"inherit readonly", "interface Foo { inherit readonly attribute double x; };", "expected attribute, got readonly"},
		{"inherit without attribute", "interface Foo { inherit double x; };", "expected attribute, got"},
	}
	for _, tt := range errCases {
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

// --- maplike / setlike (#168) ---

// idlType writes a type reference back in IDL spelling, for the kinds the
// maplike and setlike tests use.
func idlType(ref *TypeRef) string {
	s := ref.Builtin
	if ref.Kind == NamedType {
		s = ref.Name
	}
	if ref.Nullable {
		s += "?"
	}
	return s
}

// idlMember writes an attribute or operation back in IDL spelling, prefixed
// with what declares it: "maplike: boolean has(DOMString key)".
func idlMember(t *testing.T, m Member) string {
	t.Helper()
	switch m := m.(type) {
	case *Attribute:
		ro := ""
		if m.Readonly {
			ro = "readonly "
		}
		return fmt.Sprintf("%s: %sattribute %s %s", m.DeclaredBy, ro, idlType(m.Type), m.Name)
	case *Operation:
		var params []string
		for _, p := range m.Params {
			params = append(params, idlType(p.Type)+" "+p.Name)
		}
		return fmt.Sprintf("%s: %s %s(%s)", m.DeclaredBy, idlType(m.Return), m.Name, strings.Join(params, ", "))
	}
	t.Fatalf("member %T is not an attribute or an operation", m)
	return ""
}

func TestParseMaplikeSetlike(t *testing.T) {
	cases := []struct {
		name string
		decl string
		want Member
	}{
		{
			name: "maplike",
			decl: "maplike<DOMString, long>;",
			want: &Maplike{
				KeyType:   &TypeRef{Kind: BuiltinType, Builtin: "DOMString"},
				ValueType: &TypeRef{Kind: BuiltinType, Builtin: "long"},
				Pos:       Pos{File: "test.webidl", Line: 1, Column: 15},
			},
		},
		{
			// The position is the keyword's, after `readonly`.
			name: "readonly maplike",
			decl: "readonly maplike<DOMString, Node?>;",
			want: &Maplike{
				KeyType:   &TypeRef{Kind: BuiltinType, Builtin: "DOMString"},
				ValueType: &TypeRef{Kind: NamedType, Name: "Node", Nullable: true},
				Readonly:  true,
				Pos:       Pos{File: "test.webidl", Line: 1, Column: 24},
			},
		},
		{
			name: "setlike",
			decl: "setlike<DOMString>;",
			want: &Setlike{
				ValueType: &TypeRef{Kind: BuiltinType, Builtin: "DOMString"},
				Pos:       Pos{File: "test.webidl", Line: 1, Column: 15},
			},
		},
		{
			name: "readonly setlike",
			decl: "readonly setlike<long long>;",
			want: &Setlike{
				ValueType: &TypeRef{Kind: BuiltinType, Builtin: "long long"},
				Readonly:  true,
				Pos:       Pos{File: "test.webidl", Line: 1, Column: 24},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file, errs := Parse("interface A { "+tc.decl+" };", "test.webidl")
			if len(errs) > 0 {
				t.Fatalf("unexpected errors: %v", errs)
			}
			members := file.Interfaces[0].Members
			if len(members) != 1 || !reflect.DeepEqual(members[0], tc.want) {
				t.Errorf("members = %#v, want [%#v]", members, tc.want)
			}
		})
	}

	// Like legacycaller, maplike and setlike start a declaration only at the
	// start of a member: anywhere else they still name something.
	t.Run("identifiers elsewhere", func(t *testing.T) {
		src := "interface A { undefined f(long maplike, long setlike); attribute long setlike; readonly attribute long maplike; };"
		file, errs := Parse(src, "test.webidl")
		if len(errs) > 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		members := file.Interfaces[0].Members
		if len(members) != 3 {
			t.Fatalf("got %d members, want 3", len(members))
		}
		op, ok := members[0].(*Operation)
		if !ok || len(op.Params) != 2 || op.Params[0].Name != "maplike" || op.Params[1].Name != "setlike" {
			t.Errorf("members[0] = %#v, want f with params maplike, setlike", members[0])
		}
		for i, name := range []string{"setlike", "maplike"} {
			if a, ok := members[i+1].(*Attribute); !ok || a.Name != name {
				t.Errorf("members[%d] = %#v, want attribute %s", i+1, members[i+1], name)
			}
		}
	})
}

// A declaration with the other kind's arity is reported where it goes wrong.
func TestParseMaplikeSetlikeArity(t *testing.T) {
	cases := []struct {
		name, decl, want string
	}{
		{"maplike with one type", "maplike<DOMString>;", "test.webidl:1:32: expected ,, got >"},
		{"setlike with two types", "setlike<DOMString, long>;", "test.webidl:1:32: expected >, got ,"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, errs := Parse("interface A { "+tc.decl+" };", "test.webidl")
			if len(errs) == 0 || errs[0].Error() != tc.want {
				t.Errorf("errors = %v, want first %q", errs, tc.want)
			}
		})
	}
}

// Merge replaces a maplike or setlike declaration with the members WebIDL
// declares for it, in its place, each carrying the declaration's position and
// doc comment, and DeclaredBy its keyword.
func TestMergeExpandsMaplikeSetlike(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{
			name: "maplike",
			src:  "interface A { maplike<DOMString, long>; };",
			want: []string{
				"maplike: readonly attribute unsigned long size",
				"maplike: boolean has(DOMString key)",
				"maplike: long? get(DOMString key)",
				"maplike: undefined set(DOMString key, long value)",
				"maplike: boolean delete(DOMString key)",
				"maplike: undefined clear()",
			},
		},
		{
			name: "readonly maplike",
			src:  "interface A { readonly maplike<DOMString, Node>; }; interface Node {};",
			want: []string{
				"maplike: readonly attribute unsigned long size",
				"maplike: boolean has(DOMString key)",
				"maplike: Node? get(DOMString key)",
			},
		},
		{
			// get's result is nullable once, and set's value stays as declared.
			name: "maplike of a nullable value",
			src:  "interface A { maplike<long, long?>; };",
			want: []string{
				"maplike: readonly attribute unsigned long size",
				"maplike: boolean has(long key)",
				"maplike: long? get(long key)",
				"maplike: undefined set(long key, long? value)",
				"maplike: boolean delete(long key)",
				"maplike: undefined clear()",
			},
		},
		{
			name: "setlike",
			src:  "interface A { setlike<DOMString>; };",
			want: []string{
				"setlike: readonly attribute unsigned long size",
				"setlike: boolean has(DOMString value)",
				"setlike: undefined add(DOMString value)",
				"setlike: boolean delete(DOMString value)",
				"setlike: undefined clear()",
			},
		},
		{
			name: "readonly setlike",
			src:  "interface A { readonly setlike<long>; };",
			want: []string{
				"setlike: readonly attribute unsigned long size",
				"setlike: boolean has(long value)",
			},
		},
		{
			// In its place among the members the IDL writes, which keep theirs.
			name: "beside written members",
			src:  "interface A { undefined before(); readonly setlike<long>; attribute long after; };",
			want: []string{
				": undefined before()",
				"setlike: readonly attribute unsigned long size",
				"setlike: boolean has(long value)",
				": attribute long after",
			},
		},
		{
			name: "from a partial interface",
			src:  "interface A {}; partial interface A { readonly setlike<long>; };",
			want: []string{
				"setlike: readonly attribute unsigned long size",
				"setlike: boolean has(long value)",
			},
		},
		{
			// A regular operation the interface writes by a mutator's name
			// overrides it, as CSS Font Loading's FontFaceSet writes all three.
			name: "setlike with written mutators",
			src: `interface A {
				setlike<FontFace>;
				A add(FontFace font);
				boolean delete(FontFace font);
				undefined clear();
			};
			interface FontFace {};`,
			want: []string{
				"setlike: readonly attribute unsigned long size",
				"setlike: boolean has(FontFace value)",
				": A add(FontFace font)",
				": boolean delete(FontFace font)",
				": undefined clear()",
			},
		},
		{
			// Before the declaration or after it, and only by its own name; a
			// static operation is not a regular one.
			name: "maplike with a written set",
			src:  "interface A { undefined set(DOMString name, long value); static undefined clear(); maplike<DOMString, long>; };",
			want: []string{
				": undefined set(DOMString name, long value)",
				": undefined clear()",
				"maplike: readonly attribute unsigned long size",
				"maplike: boolean has(DOMString key)",
				"maplike: long? get(DOMString key)",
				"maplike: boolean delete(DOMString key)",
				"maplike: undefined clear()",
			},
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
			var got []string
			for _, m := range file.Interfaces[0].Members {
				got = append(got, idlMember(t, m))
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("members =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(tc.want, "\n  "))
			}
		})
	}

	t.Run("position and doc", func(t *testing.T) {
		file, errs := Parse("interface A {\n  // Counts.\n  maplike<DOMString, long>;\n};", "test.webidl")
		if len(errs) > 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		if errs := Merge(file); len(errs) > 0 {
			t.Fatalf("merge errors: %v", errs)
		}
		want := Pos{File: "test.webidl", Line: 3, Column: 3}
		for _, m := range file.Interfaces[0].Members {
			var pos Pos
			switch m := m.(type) {
			case *Attribute:
				pos = m.Pos
			case *Operation:
				pos = m.Pos
			}
			if doc := memberDoc(t, m); pos != want || doc != "Counts." {
				t.Errorf("%s: at %s with doc %q, want %s and %q", idlMember(t, m), pos, doc, want, "Counts.")
			}
		}
	})
}

// WebIDL permits one maplike or setlike declaration per interface: each after
// the first, counted once partials are merged, is reported at its position.
func TestMergeRejectsSecondMaplikeSetlike(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{
			name: "two maplikes",
			src:  "interface A { maplike<long, long>; maplike<long, long>; };",
			want: []string{"test.webidl:1:36: interface A declares a second maplike or setlike (the first is at test.webidl:1:15); WebIDL permits one"},
		},
		{
			name: "a maplike and a setlike, and a third",
			src:  "interface A { maplike<long, long>;\nreadonly setlike<long>;\nsetlike<long>; };",
			want: []string{
				"test.webidl:2:10: interface A declares a second maplike or setlike (the first is at test.webidl:1:15); WebIDL permits one",
				"test.webidl:3:1: interface A declares a second maplike or setlike (the first is at test.webidl:1:15); WebIDL permits one",
			},
		},
		{
			name: "one in a partial interface",
			src:  "interface A { setlike<long>; };\npartial interface A { maplike<long, long>; };",
			want: []string{"test.webidl:2:23: interface A declares a second maplike or setlike (the first is at test.webidl:1:15); WebIDL permits one"},
		},
		{
			name: "one on each of two interfaces",
			src:  "interface P { setlike<long>; };\ninterface C { maplike<long, long>; };",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file, errs := Parse(tc.src, "test.webidl")
			if len(errs) > 0 {
				t.Fatalf("unexpected errors: %v", errs)
			}
			var got []string
			for _, e := range Merge(file) {
				got = append(got, e.Error())
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("merge errors = %q, want %q", got, tc.want)
			}
		})
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
	if !strings.HasPrefix(errMsg, "1:1: ") {
		t.Errorf("error should start with line:column and no filename, got: %s", errMsg)
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

// An identifier list is one extended attribute, its commas inside the
// parentheses (#722) — written as webref writes it, over several lines.
func TestParseExtAttrIdentifierList(t *testing.T) {
	src := `[Exposed=(Window,Worker),
 Serializable,
 LegacyWindowAlias=(SVGMatrix,WebKitCSSMatrix)]
interface DOMMatrix {};`
	file, errs := Parse(src, "geometry.idl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	var got []ExtAttr
	for _, ea := range file.Interfaces[0].ExtAttrs {
		got = append(got, *ea)
	}
	want := []ExtAttr{
		{Name: "Exposed", Value: "(Window,Worker)"},
		{Name: "Serializable"},
		{Name: "LegacyWindowAlias", Value: "(SVGMatrix,WebKitCSSMatrix)"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("ExtAttrs = %+v, want %+v", got, want)
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

// TestParseNegativeLiterals pins the negative literals a `-` still starts now
// that one may also lead an identifier (#719): `-Infinity` used to lex as the
// integer `-` followed by the identifier `Infinity`, so a constant or default
// written with it did not parse.
func TestParseNegativeLiterals(t *testing.T) {
	src := `interface Foo {
		const unrestricted double NEG = -Infinity;
		const long MINUS_ONE = -1;
		undefined f(optional unrestricted double a = -Infinity, optional double b = -1.5, optional long c = -1);
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	members := file.Interfaces[0].Members
	if len(members) != 3 {
		t.Fatalf("expected 3 members, got %d", len(members))
	}
	var got []string
	for _, m := range members[:2] {
		got = append(got, m.(*Const).Value)
	}
	for _, p := range members[2].(*Operation).Params {
		got = append(got, p.Default)
	}
	if want := []string{"-Infinity", "-1", "-Infinity", "-1.5", "-1"}; !slices.Equal(got, want) {
		t.Errorf("values = %q, want %q", got, want)
	}
}

// TestParseHyphenatedNames pins #719: an attribute, an operation and an
// argument may each be named by an identifier containing `-`, or led by one.
func TestParseHyphenatedNames(t *testing.T) {
	src := `interface Foo {
		attribute DOMString margin-top;
		readonly attribute DOMString -webkit-foo;
		undefined set-property(DOMString property-name, optional long a-1 = -1);
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	members := file.Interfaces[0].Members
	if len(members) != 3 {
		t.Fatalf("expected 3 members, got %d", len(members))
	}
	if got := members[0].(*Attribute).Name; got != "margin-top" {
		t.Errorf("attribute name = %q, want margin-top", got)
	}
	if got := members[1].(*Attribute).Name; got != "-webkit-foo" {
		t.Errorf("attribute name = %q, want -webkit-foo", got)
	}
	op := members[2].(*Operation)
	if op.Name != "set-property" {
		t.Errorf("operation name = %q, want set-property", op.Name)
	}
	var params []string
	for _, p := range op.Params {
		params = append(params, p.Name)
	}
	if want := []string{"property-name", "a-1"}; !slices.Equal(params, want) {
		t.Errorf("params = %q, want %q", params, want)
	}
	if got := op.Params[1].Default; got != "-1" {
		t.Errorf("default = %q, want -1", got)
	}
}

// TestParseWebrefCSSPageDescriptors parses the CSSPageDescriptors interface
// verbatim from webref's cssom.idl (@webref/idl 3.83.0, lines 113–129), which
// declares each margin both camel-cased and dashed. Before #719, line 120's
// `margin-top` failed to parse, and with it every file bound together.
func TestParseWebrefCSSPageDescriptors(t *testing.T) {
	src := `[Exposed=Window]
interface CSSPageDescriptors : CSSStyleDeclaration {
  attribute [LegacyNullToEmptyString] CSSOMString margin;
  attribute [LegacyNullToEmptyString] CSSOMString marginTop;
  attribute [LegacyNullToEmptyString] CSSOMString marginRight;
  attribute [LegacyNullToEmptyString] CSSOMString marginBottom;
  attribute [LegacyNullToEmptyString] CSSOMString marginLeft;
  attribute [LegacyNullToEmptyString] CSSOMString margin-top;
  attribute [LegacyNullToEmptyString] CSSOMString margin-right;
  attribute [LegacyNullToEmptyString] CSSOMString margin-bottom;
  attribute [LegacyNullToEmptyString] CSSOMString margin-left;
  attribute [LegacyNullToEmptyString] CSSOMString size;
  attribute [LegacyNullToEmptyString] CSSOMString pageOrientation;
  attribute [LegacyNullToEmptyString] CSSOMString page-orientation;
  attribute [LegacyNullToEmptyString] CSSOMString marks;
  attribute [LegacyNullToEmptyString] CSSOMString bleed;
};`
	file, errs := Parse(src, "cssom.idl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	var names []string
	for _, m := range file.Interfaces[0].Members {
		attr := m.(*Attribute)
		if attr.Type.Name != "CSSOMString" {
			t.Errorf("%s: type %q, want CSSOMString", attr.Name, attr.Type.Name)
		}
		names = append(names, attr.Name)
	}
	want := []string{
		"margin", "marginTop", "marginRight", "marginBottom", "marginLeft",
		"margin-top", "margin-right", "margin-bottom", "margin-left",
		"size", "pageOrientation", "page-orientation", "marks", "bleed",
	}
	if !slices.Equal(names, want) {
		t.Errorf("attributes = %q\nwant         %q", names, want)
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
			// The same input spelled partial interface: an interface mixin is a
			// mixin, not an interface, so the partial finds no interface and the
			// mixin keeps only its own member.
			name: "partial interface on an interface mixin",
			src:  "partial interface X { undefined b(); };\ninterface mixin X { undefined a(); };\ninterface T {};\nT includes X;",
			want: []string{
				"test.webidl:1:9: partial interface X: X is not an interface in the bound IDL files",
			},
			members: map[string]int{"T": 1, "X": 1},
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
			name: "partial namespace",
			src:  "namespace Base { undefined a(); };\npartial namespace Unknown { undefined extra(); };",
			want: []string{
				"test.webidl:2:9: partial namespace Unknown: Unknown is not a namespace in the bound IDL files",
			},
			members: map[string]int{"Base": 1},
		},
		{
			// A namespace is not an interface, in either direction.
			name: "partial namespace naming an interface",
			src:  "interface X { undefined a(); };\npartial namespace X { undefined b(); };",
			want: []string{
				"test.webidl:2:9: partial namespace X: X is not a namespace in the bound IDL files",
			},
			members: map[string]int{"X": 1},
		},
		{
			name: "partial interface naming a namespace",
			src:  "namespace X { undefined a(); };\npartial interface X { undefined b(); };",
			want: []string{
				"test.webidl:2:9: partial interface X: X is not an interface in the bound IDL files",
			},
			members: map[string]int{"X": 1},
		},
		{
			name: "includes with no interface",
			src:  "interface mixin Mix { undefined extra(); };\nMissing includes Mix;",
			want: []string{
				"test.webidl:2:1: Missing includes Mix: Missing is not an interface in the bound IDL files",
			},
		},
		{
			// The left side is an interface: an interface mixin of the name is not.
			name: "includes with an interface mixin as its interface",
			src:  "interface mixin X { undefined a(); };\ninterface mixin M { undefined c(); };\nX includes M;",
			want: []string{
				"test.webidl:3:1: X includes M: X is not an interface in the bound IDL files",
			},
			members: map[string]int{"X": 1},
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
				for _, ns := range file.Namespaces {
					if ns.Name == name {
						count = len(ns.Members)
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
			// Merge expands a maplike before recording, so the members it
			// declares are regular ones like any the IDL writes.
			name: "member a maplike declares",
			src: `interface P { maplike<DOMString, long>; };
			interface C : P { boolean has(long key); };`,
			want: map[string][]string{"P": {"has"}},
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
			// An interface mixin is not an interface, so it is no parent either.
			name: "parent is an interface mixin",
			src: `interface mixin M { undefined m(); };
			interface C : M { undefined m(); };`,
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

// TestMergeRecordsOverriddenBuiltIns pins Interface.OverriddenBuiltIns (#697):
// an object implementing an interface that carries [LegacyOverrideBuiltIns]
// has its supported named properties as its own, over every member of its
// prototype chain, so Merge records the regular attributes and operations of
// each interface in that interface's inheritance chain — its ancestors, itself
// and its descendants — but not the [LegacyUnforgeable] ones, which live on the
// object. want lists, per interface, the identifiers recorded; an interface it
// omits records none.
func TestMergeRecordsOverriddenBuiltIns(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want map[string][]string
	}{
		{
			name: "on the definition, through every ancestor",
			src: `interface Node { undefined remove(); };
			interface Element : Node { attribute DOMString id; };
			interface HTMLElement : Element { attribute DOMString title; };
			[Exposed=Window, LegacyOverrideBuiltIns, LegacyUnenumerableNamedProperties]
			interface HTMLFormElement : HTMLElement {
				attribute DOMString action;
				undefined submit();
				getter Element (unsigned long index);
				getter (RadioNodeList or Element) (DOMString name);
			};`,
			want: map[string][]string{
				"Node":            {"remove"},
				"Element":         {"id"},
				"HTMLElement":     {"title"},
				"HTMLFormElement": {"action", "submit"},
			},
		},
		{
			// HTML puts it on a partial Document; the partial's members count
			// as well as the definition's.
			name: "on a partial interface",
			src: `interface Node { undefined remove(); };
			interface Document : Node { attribute DOMString cookie; };
			[LegacyOverrideBuiltIns]
			partial interface Document { attribute DOMString title; getter object (DOMString name); };`,
			want: map[string][]string{"Node": {"remove"}, "Document": {"cookie", "title"}},
		},
		{
			name: "unforgeable members",
			src: `interface Location {};
			[LegacyOverrideBuiltIns] interface Document {
				[PutForwards=href, LegacyUnforgeable] readonly attribute Location? location;
				[LegacyUnforgeable] undefined reload();
				attribute DOMString cookie;
			};`,
			want: map[string][]string{"Document": {"cookie"}},
		},
		{
			// An XMLDocument is a Document, named properties included.
			name: "descendant",
			src: `[LegacyOverrideBuiltIns] interface Document { attribute DOMString cookie; };
			interface XMLDocument : Document { undefined load(DOMString url); };`,
			want: map[string][]string{"Document": {"cookie"}, "XMLDocument": {"load"}},
		},
		{
			// Neither a sibling nor an interface sharing only the root is ever
			// one of the form's interfaces.
			name: "sibling and an interface sharing the root",
			src: `interface EventTarget { undefined addEventListener(DOMString type); };
			interface Node : EventTarget { undefined remove(); };
			interface Element : Node {};
			interface HTMLElement : Element {};
			[LegacyOverrideBuiltIns] interface HTMLFormElement : HTMLElement {};
			interface HTMLDivElement : HTMLElement { attribute DOMString align; };
			interface Window : EventTarget { readonly attribute DOMString name; };`,
			want: map[string][]string{"EventTarget": {"addEventListener"}, "Node": {"remove"}},
		},
		{
			// Recorded per including interface: a mixin member lives on each
			// includer's prototype, and only Element's is in the chain.
			name: "mixin members",
			src: `interface Node {};
			interface Element : Node {};
			interface DocumentFragment : Node {};
			[LegacyOverrideBuiltIns] interface HTMLFormElement : Element {};
			interface mixin ParentNode { Element? querySelector(DOMString selectors); };
			Element includes ParentNode;
			DocumentFragment includes ParentNode;`,
			want: map[string][]string{"Element": {"querySelector"}},
		},
		{
			// Statics, constants and constructors are reached through the
			// interface object, and an anonymous special operation names no
			// property.
			name: "statics, constant, constructor and anonymous specials",
			src: `[LegacyOverrideBuiltIns] interface F {
				static undefined s();
				static attribute long t;
				const long C = 1;
				constructor();
				getter long (unsigned long index);
				setter undefined (DOMString name, long value);
				deleter undefined (DOMString name);
				undefined m();
			};`,
			want: map[string][]string{"F": {"m"}},
		},
		{
			name: "DOMStringMap",
			src: `[Exposed=Window, LegacyOverrideBuiltIns]
			interface DOMStringMap {
				getter DOMString (DOMString name);
				setter undefined (DOMString name, DOMString value);
				deleter undefined (DOMString name);
			};`,
		},
		{
			name: "no interface carries it",
			src: `interface Node { undefined remove(); };
			[Exposed=Window] interface Element : Node { attribute DOMString id; };
			[Exposed=Window] partial interface Element { attribute DOMString title; };`,
		},
		{
			name: "parent not in the file",
			src:  `[LegacyOverrideBuiltIns] interface F : Missing { attribute long a; };`,
			want: map[string][]string{"F": {"a"}},
		},
		{
			name: "inheritance cycle",
			src: `[LegacyOverrideBuiltIns] interface A : B { undefined m(); };
			interface B : A { undefined n(); };`,
			want: map[string][]string{"A": {"m"}, "B": {"n"}},
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
				got := slices.Sorted(maps.Keys(iface.OverriddenBuiltIns))
				if want := tc.want[iface.Name]; !slices.Equal(got, want) {
					t.Errorf("%s.OverriddenBuiltIns = %q, want %q", iface.Name, got, want)
				}
			}
		})
	}
}

// Merge maps each identifier a [LegacyWindowAlias] names to the interface it
// annotates, from the whole input (#722).
func TestMergeRecordsWindowAliases(t *testing.T) {
	cases := []struct {
		name string
		srcs []string // parsed one by one, then appended, as `bind` does
		want map[string]string
	}{
		{
			name: "one identifier and a list, declared in another file than the one using them",
			srcs: []string{
				`[Exposed=Window, LegacyWindowAlias=SVGPoint] interface DOMPoint {};
				[Exposed=Window, LegacyWindowAlias=( SVGMatrix,
				  WebKitCSSMatrix )] interface DOMMatrix {};`,
				`interface SVGGeometryElement { SVGPoint getPointAtLength(float distance); };`,
			},
			want: map[string]string{"SVGPoint": "DOMPoint", "SVGMatrix": "DOMMatrix", "WebKitCSSMatrix": "DOMMatrix"},
		},
		{
			// WebIDL forbids two interfaces naming one alias; the first keeps it.
			name: "the first interface to name an identifier keeps it",
			srcs: []string{`[LegacyWindowAlias=X] interface A {}; [LegacyWindowAlias=X] interface B {};`},
			want: map[string]string{"X": "A"},
		},
		{
			// A mixin is not an interface, and a partial interface does not
			// carry the attribute.
			name: "neither a mixin nor a partial interface",
			srcs: []string{`[LegacyWindowAlias=M] interface mixin Mix {};
			interface P {};
			[LegacyWindowAlias=Q] partial interface P {};`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file := &File{}
			for i, src := range tc.srcs {
				parsed, errs := Parse(src, fmt.Sprintf("%d.webidl", i))
				if len(errs) > 0 {
					t.Fatalf("unexpected errors: %v", errs)
				}
				file.Append(parsed)
			}
			if errs := Merge(file); len(errs) > 0 {
				t.Fatalf("merge errors: %v", errs)
			}
			if !maps.Equal(file.WindowAliases, tc.want) {
				t.Errorf("WindowAliases = %v, want %v", file.WindowAliases, tc.want)
			}
		})
	}
}

// TestFileAppendCopiesEveryField walks File's fields by reflection rather than
// listing them, so a field added to File that Append does not copy fails here
// (#656: bind's hand-kept list dropped PartialDicts).
func TestFileAppendCopiesEveryField(t *testing.T) {
	// Merge sets these from the merged definitions, after Append has combined
	// them, so Append has nothing of theirs to combine.
	mergeDerived := map[string]bool{"WindowAliases": true}
	dst, src := &File{}, &File{}
	dv, sv := reflect.ValueOf(dst).Elem(), reflect.ValueOf(src).Elem()
	typ := dv.Type()
	for i := 0; i < typ.NumField(); i++ {
		if mergeDerived[typ.Field(i).Name] {
			continue
		}
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
		if !mergeDerived[typ.Field(i).Name] {
			want[i] = [2]any{dv.Field(i).Index(0).Interface(), sv.Field(i).Index(0).Interface()}
		}
	}

	dst.Append(src)

	for i := 0; i < typ.NumField(); i++ {
		name, got := typ.Field(i).Name, dv.Field(i)
		if mergeDerived[name] {
			continue
		}
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
	case *Maplike:
		return m.Doc
	case *Setlike:
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
		// map.
		maplike<DOMString, long>;
		// set.
		readonly setlike<long>;
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	want := []string{"ro.", "rw.", "st.", "op.", "sop.", "ov1.", "ov2.", "get.", "ctor.", "const.", "ce.", "ce2.", "str.", "str2.", "map.", "set."}
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
