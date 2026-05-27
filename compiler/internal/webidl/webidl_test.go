package webidl

import (
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

	Merge(file)

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
	Merge(file)
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
	Merge(file)
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
	strAttr := members[2].(*Attribute)
	if strAttr.Doc != "stringifier" {
		t.Errorf("expected stringifier doc, got %q", strAttr.Doc)
	}
	strOp := members[3].(*Operation)
	if strOp.Special != "stringifier" {
		t.Errorf("expected special 'stringifier', got %q", strOp.Special)
	}
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

func TestMergePartialDictUnmatched(t *testing.T) {
	// Partial dictionary with no matching base is silently dropped
	src := `dictionary Base {
		DOMString name;
	};
	partial dictionary Unknown {
		long extra;
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	Merge(file)
	// Base should still have only its original member
	if len(file.Dictionaries[0].Members) != 1 {
		t.Errorf("expected 1 member in Base, got %d", len(file.Dictionaries[0].Members))
	}
}

func TestMergePartialInterfaceUnmatched(t *testing.T) {
	// Partial interface with no matching base is silently dropped
	src := `interface Base {
		readonly attribute DOMString name;
	};
	partial interface Unknown {
		void extra();
	};`
	file, errs := Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	Merge(file)
	if len(file.Interfaces[0].Members) != 1 {
		t.Errorf("expected 1 member in Base, got %d", len(file.Interfaces[0].Members))
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
