package wit

import (
	"testing"
)

// --- Lexer tests ---

func TestLexerKeywords(t *testing.T) {
	src := `package interface world import export use type record variant enum flags resource func static constructor as`
	lex := NewLexer(src, "test.wit")

	expected := []TokenKind{
		TokenPackage, TokenInterface, TokenWorld, TokenImport, TokenExport,
		TokenUse, TokenType, TokenRecord, TokenVariant, TokenEnum,
		TokenFlags, TokenResource, TokenFunc, TokenStatic, TokenConstructor, TokenAs,
		TokenEOF,
	}
	for i, want := range expected {
		tok := lex.Next()
		if tok.Kind != want {
			t.Errorf("token %d: got %s, want %s", i, tok.Kind, want)
		}
	}
}

func TestLexerBuiltinTypes(t *testing.T) {
	src := `u8 u16 u32 u64 s8 s16 s32 s64 f32 f64 bool char string list option result tuple own borrow`
	lex := NewLexer(src, "test.wit")

	expected := []TokenKind{
		TokenU8, TokenU16, TokenU32, TokenU64,
		TokenS8, TokenS16, TokenS32, TokenS64,
		TokenF32, TokenF64, TokenBool, TokenChar, TokenString,
		TokenList, TokenOption, TokenResult, TokenTuple, TokenOwn, TokenBorrow,
		TokenEOF,
	}
	for i, want := range expected {
		tok := lex.Next()
		if tok.Kind != want {
			t.Errorf("token %d: got %s, want %s", i, tok.Kind, want)
		}
	}
}

func TestLexerPunctuation(t *testing.T) {
	src := `{ } ( ) < > : ; , . = / @ * ->`
	lex := NewLexer(src, "test.wit")

	expected := []TokenKind{
		TokenLBrace, TokenRBrace, TokenLParen, TokenRParen,
		TokenLAngle, TokenRAngle, TokenColon, TokenSemicolon,
		TokenComma, TokenDot, TokenEquals, TokenSlash, TokenAt, TokenStar,
		TokenArrow, TokenEOF,
	}
	for i, want := range expected {
		tok := lex.Next()
		if tok.Kind != want {
			t.Errorf("token %d: got %s, want %s", i, tok.Kind, want)
		}
	}
}

func TestLexerKebabIdent(t *testing.T) {
	src := `my-function-name path-open descriptor-type`
	lex := NewLexer(src, "test.wit")

	expected := []string{"my-function-name", "path-open", "descriptor-type"}
	for i, want := range expected {
		tok := lex.Next()
		if tok.Kind != TokenIdent {
			t.Errorf("token %d: got kind %s, want identifier", i, tok.Kind)
		}
		if tok.Value != want {
			t.Errorf("token %d: got value %q, want %q", i, tok.Value, want)
		}
	}
}

func TestLexerLineComments(t *testing.T) {
	src := "record // this is a comment\nvariant"
	lex := NewLexer(src, "test.wit")

	tok1 := lex.Next()
	if tok1.Kind != TokenRecord {
		t.Errorf("expected record, got %s", tok1.Kind)
	}
	tok2 := lex.Next()
	if tok2.Kind != TokenVariant {
		t.Errorf("expected variant, got %s", tok2.Kind)
	}
}

func TestLexerBlockComments(t *testing.T) {
	src := "record /* block comment */ variant"
	lex := NewLexer(src, "test.wit")

	tok1 := lex.Next()
	if tok1.Kind != TokenRecord {
		t.Errorf("expected record, got %s", tok1.Kind)
	}
	tok2 := lex.Next()
	if tok2.Kind != TokenVariant {
		t.Errorf("expected variant, got %s", tok2.Kind)
	}
}

func TestLexerPositions(t *testing.T) {
	src := "record\nvariant"
	lex := NewLexer(src, "test.wit")

	tok1 := lex.Next()
	if tok1.Pos.Line != 1 || tok1.Pos.Column != 1 {
		t.Errorf("token 1 pos: got %d:%d, want 1:1", tok1.Pos.Line, tok1.Pos.Column)
	}
	tok2 := lex.Next()
	if tok2.Pos.Line != 2 || tok2.Pos.Column != 1 {
		t.Errorf("token 2 pos: got %d:%d, want 2:1", tok2.Pos.Line, tok2.Pos.Column)
	}
}

// --- Parser tests ---

func TestParsePackage(t *testing.T) {
	src := `package wasi:filesystem@0.2.0;`
	file, errs := Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	if file.Package == nil {
		t.Fatal("expected package declaration")
	}
	if file.Package.Namespace != "wasi" {
		t.Errorf("namespace: got %q, want %q", file.Package.Namespace, "wasi")
	}
	if file.Package.Name != "filesystem" {
		t.Errorf("name: got %q, want %q", file.Package.Name, "filesystem")
	}
	if file.Package.Version != "0.2.0" {
		t.Errorf("version: got %q, want %q", file.Package.Version, "0.2.0")
	}
}

func TestParseEmptyInterface(t *testing.T) {
	src := `interface empty {}`
	file, errs := Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	if len(file.Interfaces) != 1 {
		t.Fatalf("expected 1 interface, got %d", len(file.Interfaces))
	}
	if file.Interfaces[0].Name != "empty" {
		t.Errorf("interface name: got %q, want %q", file.Interfaces[0].Name, "empty")
	}
}

func TestParseRecord(t *testing.T) {
	src := `
interface types {
    record point {
        x: f64,
        y: f64,
    }
}
`
	file, errs := Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	if len(file.Interfaces) != 1 {
		t.Fatalf("expected 1 interface, got %d", len(file.Interfaces))
	}
	items := file.Interfaces[0].Items
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	rec, ok := items[0].(*Record)
	if !ok {
		t.Fatalf("expected Record, got %T", items[0])
	}
	if rec.Name != "point" {
		t.Errorf("record name: got %q, want %q", rec.Name, "point")
	}
	if len(rec.Fields) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(rec.Fields))
	}
	if rec.Fields[0].Name != "x" || rec.Fields[0].Type.Builtin != "f64" {
		t.Errorf("field 0: got %q:%q", rec.Fields[0].Name, rec.Fields[0].Type.Builtin)
	}
	if rec.Fields[1].Name != "y" || rec.Fields[1].Type.Builtin != "f64" {
		t.Errorf("field 1: got %q:%q", rec.Fields[1].Name, rec.Fields[1].Type.Builtin)
	}
}

func TestParseEnum(t *testing.T) {
	src := `
interface types {
    enum color {
        red,
        green,
        blue,
    }
}
`
	file, errs := Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	items := file.Interfaces[0].Items
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	enum, ok := items[0].(*Enum)
	if !ok {
		t.Fatalf("expected Enum, got %T", items[0])
	}
	if enum.Name != "color" {
		t.Errorf("enum name: got %q, want %q", enum.Name, "color")
	}
	if len(enum.Cases) != 3 {
		t.Fatalf("expected 3 cases, got %d", len(enum.Cases))
	}
	expected := []string{"red", "green", "blue"}
	for i, want := range expected {
		if enum.Cases[i] != want {
			t.Errorf("case %d: got %q, want %q", i, enum.Cases[i], want)
		}
	}
}

func TestParseVariant(t *testing.T) {
	src := `
interface types {
    variant shape {
        circle(f64),
        rectangle(rect),
        none,
    }
}
`
	file, errs := Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	items := file.Interfaces[0].Items
	v, ok := items[0].(*Variant)
	if !ok {
		t.Fatalf("expected Variant, got %T", items[0])
	}
	if v.Name != "shape" {
		t.Errorf("variant name: got %q", v.Name)
	}
	if len(v.Cases) != 3 {
		t.Fatalf("expected 3 cases, got %d", len(v.Cases))
	}
	if v.Cases[0].Name != "circle" || v.Cases[0].Type == nil || v.Cases[0].Type.Builtin != "f64" {
		t.Errorf("case 0 unexpected")
	}
	if v.Cases[1].Name != "rectangle" || v.Cases[1].Type == nil || v.Cases[1].Type.Name != "rect" {
		t.Errorf("case 1 unexpected")
	}
	if v.Cases[2].Name != "none" || v.Cases[2].Type != nil {
		t.Errorf("case 2: expected no payload")
	}
}

func TestParseFlags(t *testing.T) {
	src := `
interface types {
    flags open-flags {
        read,
        write,
        append,
    }
}
`
	file, errs := Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	items := file.Interfaces[0].Items
	f, ok := items[0].(*Flags)
	if !ok {
		t.Fatalf("expected Flags, got %T", items[0])
	}
	if f.Name != "open-flags" {
		t.Errorf("flags name: got %q", f.Name)
	}
	if len(f.Flags) != 3 {
		t.Fatalf("expected 3 flags, got %d", len(f.Flags))
	}
}

func TestParseResource(t *testing.T) {
	src := `
interface fs {
    resource descriptor {
        constructor(path: string);
        read: func(len: u64) -> result<list<u8>, u32>;
        static open: func(path: string) -> result<descriptor, u32>;
    }
}
`
	file, errs := Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	items := file.Interfaces[0].Items
	r, ok := items[0].(*Resource)
	if !ok {
		t.Fatalf("expected Resource, got %T", items[0])
	}
	if r.Name != "descriptor" {
		t.Errorf("resource name: got %q", r.Name)
	}
	if len(r.Methods) != 3 {
		t.Fatalf("expected 3 methods, got %d", len(r.Methods))
	}
	if r.Methods[0].Kind != FuncConstructor {
		t.Errorf("method 0: expected constructor, got %v", r.Methods[0].Kind)
	}
	if r.Methods[1].Kind != FuncMethod {
		t.Errorf("method 1: expected method, got %v", r.Methods[1].Kind)
	}
	if r.Methods[1].Name != "read" {
		t.Errorf("method 1 name: got %q", r.Methods[1].Name)
	}
	if r.Methods[2].Kind != FuncStatic {
		t.Errorf("method 2: expected static, got %v", r.Methods[2].Kind)
	}
}

func TestParseFreeFunc(t *testing.T) {
	src := `
interface clocks {
    now: func() -> u64;
    sleep: func(duration: u64);
}
`
	file, errs := Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	items := file.Interfaces[0].Items
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	fn1, ok := items[0].(*Func)
	if !ok {
		t.Fatalf("expected Func, got %T", items[0])
	}
	if fn1.Name != "now" {
		t.Errorf("func name: got %q", fn1.Name)
	}
	if fn1.Results == nil || fn1.Results.Anon == nil || fn1.Results.Anon.Builtin != "u64" {
		t.Errorf("func result unexpected")
	}
	fn2, ok := items[1].(*Func)
	if !ok {
		t.Fatalf("expected Func, got %T", items[1])
	}
	if fn2.Name != "sleep" {
		t.Errorf("func name: got %q", fn2.Name)
	}
	if len(fn2.Params) != 1 {
		t.Fatalf("expected 1 param, got %d", len(fn2.Params))
	}
	if fn2.Params[0].Name != "duration" {
		t.Errorf("param name: got %q", fn2.Params[0].Name)
	}
}

func TestParseTypeAlias(t *testing.T) {
	src := `
interface types {
    type error-code = u32;
}
`
	file, errs := Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	items := file.Interfaces[0].Items
	a, ok := items[0].(*TypeAlias)
	if !ok {
		t.Fatalf("expected TypeAlias, got %T", items[0])
	}
	if a.Name != "error-code" {
		t.Errorf("alias name: got %q", a.Name)
	}
	if a.Target.Builtin != "u32" {
		t.Errorf("alias target: got %q", a.Target.Builtin)
	}
}

func TestParseParameterizedTypes(t *testing.T) {
	src := `
interface types {
    get-data: func() -> list<u8>;
    maybe-get: func() -> option<string>;
    try-get: func() -> result<u32, string>;
    get-pair: func() -> tuple<u32, string>;
}
`
	file, errs := Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	items := file.Interfaces[0].Items
	if len(items) != 4 {
		t.Fatalf("expected 4 items, got %d", len(items))
	}

	// list<u8>
	fn0 := items[0].(*Func)
	if fn0.Results.Anon.Kind != ListType {
		t.Errorf("func 0: expected list type, got %v", fn0.Results.Anon.Kind)
	}

	// option<string>
	fn1 := items[1].(*Func)
	if fn1.Results.Anon.Kind != OptionType {
		t.Errorf("func 1: expected option type, got %v", fn1.Results.Anon.Kind)
	}

	// result<u32, string>
	fn2 := items[2].(*Func)
	if fn2.Results.Anon.Kind != ResultType {
		t.Errorf("func 2: expected result type, got %v", fn2.Results.Anon.Kind)
	}
	if fn2.Results.Anon.Ok == nil || fn2.Results.Anon.Ok.Builtin != "u32" {
		t.Errorf("func 2: expected ok=u32")
	}
	if fn2.Results.Anon.Err == nil || fn2.Results.Anon.Err.Builtin != "string" {
		t.Errorf("func 2: expected err=string")
	}

	// tuple<u32, string>
	fn3 := items[3].(*Func)
	if fn3.Results.Anon.Kind != TupleType {
		t.Errorf("func 3: expected tuple type, got %v", fn3.Results.Anon.Kind)
	}
	if len(fn3.Results.Anon.Elements) != 2 {
		t.Errorf("func 3: expected 2 tuple elements, got %d", len(fn3.Results.Anon.Elements))
	}
}

func TestParseWorld(t *testing.T) {
	src := `
world my-world {
    import my-iface;
    export my-other-iface;
}
`
	file, errs := Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	if len(file.Worlds) != 1 {
		t.Fatalf("expected 1 world, got %d", len(file.Worlds))
	}
	w := file.Worlds[0]
	if w.Name != "my-world" {
		t.Errorf("world name: got %q", w.Name)
	}
	if len(w.Imports) != 1 {
		t.Errorf("expected 1 import, got %d", len(w.Imports))
	}
	if len(w.Exports) != 1 {
		t.Errorf("expected 1 export, got %d", len(w.Exports))
	}
}

func TestParseUseStatement(t *testing.T) {
	src := `
interface types {
    use wasi:io/streams.{input-stream, output-stream};
}
`
	file, errs := Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	items := file.Interfaces[0].Items
	u, ok := items[0].(*Use)
	if !ok {
		t.Fatalf("expected Use, got %T", items[0])
	}
	if len(u.Names) != 2 {
		t.Fatalf("expected 2 names, got %d", len(u.Names))
	}
	if u.Names[0].Name != "input-stream" {
		t.Errorf("name 0: got %q", u.Names[0].Name)
	}
	if u.Names[1].Name != "output-stream" {
		t.Errorf("name 1: got %q", u.Names[1].Name)
	}
}

func TestParseResourceWithoutBody(t *testing.T) {
	src := `
interface types {
    resource handle;
}
`
	file, errs := Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	items := file.Interfaces[0].Items
	r, ok := items[0].(*Resource)
	if !ok {
		t.Fatalf("expected Resource, got %T", items[0])
	}
	if r.Name != "handle" {
		t.Errorf("resource name: got %q", r.Name)
	}
	if len(r.Methods) != 0 {
		t.Errorf("expected 0 methods, got %d", len(r.Methods))
	}
}

func TestParseOwnBorrow(t *testing.T) {
	src := `
interface types {
    take: func(r: own<my-resource>);
    peek: func(r: borrow<my-resource>);
}
`
	file, errs := Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	items := file.Interfaces[0].Items
	fn0 := items[0].(*Func)
	if fn0.Params[0].Type.Kind != OwnType {
		t.Errorf("expected own type, got %v", fn0.Params[0].Type.Kind)
	}
	fn1 := items[1].(*Func)
	if fn1.Params[0].Type.Kind != BorrowType {
		t.Errorf("expected borrow type, got %v", fn1.Params[0].Type.Kind)
	}
}

func TestLexerDocComment(t *testing.T) {
	src := "/// A single-line doc comment.\nrecord"
	lex := NewLexer(src, "test.wit")

	tok1 := lex.Next()
	if tok1.Kind != TokenDocComment {
		t.Errorf("expected doc-comment, got %s", tok1.Kind)
	}
	if tok1.Value != "A single-line doc comment." {
		t.Errorf("doc value: got %q", tok1.Value)
	}
	tok2 := lex.Next()
	if tok2.Kind != TokenRecord {
		t.Errorf("expected record, got %s", tok2.Kind)
	}
}

func TestLexerMultiLineDocComment(t *testing.T) {
	src := "/// Line 1.\n/// Line 2.\nrecord"
	lex := NewLexer(src, "test.wit")

	tok1 := lex.Next()
	if tok1.Kind != TokenDocComment {
		t.Errorf("expected doc-comment, got %s", tok1.Kind)
	}
	if tok1.Value != "Line 1.\nLine 2." {
		t.Errorf("doc value: got %q, want %q", tok1.Value, "Line 1.\nLine 2.")
	}
}

func TestParseDocComment(t *testing.T) {
	src := `
interface types {
    /// A point in 2D space.
    record point {
        x: f64,
        y: f64,
    }
}
`
	file, errs := Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	items := file.Interfaces[0].Items
	rec, ok := items[0].(*Record)
	if !ok {
		t.Fatalf("expected Record, got %T", items[0])
	}
	if rec.Doc != "A point in 2D space." {
		t.Errorf("doc: got %q, want %q", rec.Doc, "A point in 2D space.")
	}
}

// --- Coverage gap tests ---

func TestLexerPeekN(t *testing.T) {
	src := `record variant enum`
	lex := NewLexer(src, "test.wit")

	tok0 := lex.PeekN(0)
	if tok0.Kind != TokenRecord {
		t.Errorf("PeekN(0): got %s, want record", tok0.Kind)
	}
	tok1 := lex.PeekN(1)
	if tok1.Kind != TokenVariant {
		t.Errorf("PeekN(1): got %s, want variant", tok1.Kind)
	}
	tok2 := lex.PeekN(2)
	if tok2.Kind != TokenEnum {
		t.Errorf("PeekN(2): got %s, want enum", tok2.Kind)
	}
	// Past end returns EOF
	tokN := lex.PeekN(100)
	if tokN.Kind != TokenEOF {
		t.Errorf("PeekN(100): got %s, want EOF", tokN.Kind)
	}
}

func TestTokenStringUnknown(t *testing.T) {
	// Known token
	s := TokenRecord.String()
	if s != "record" {
		t.Errorf("TokenRecord.String() = %q, want %q", s, "record")
	}
	// Unknown token kind falls back to numeric
	unknown := TokenKind(9999)
	s2 := unknown.String()
	if s2 != "token(9999)" {
		t.Errorf("unknown.String() = %q, want %q", s2, "token(9999)")
	}
}

func TestParseWorldWithInlineFunc(t *testing.T) {
	src := `
world my-world {
    import log: func(msg: string);
    export run: func() -> u32;
}
`
	file, errs := Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	w := file.Worlds[0]
	if len(w.Imports) != 1 {
		t.Fatalf("expected 1 import, got %d", len(w.Imports))
	}
	imp := w.Imports[0]
	if imp.Func == nil {
		t.Fatal("expected inline func import")
	}
	if imp.Func.Name != "log" {
		t.Errorf("import func name: got %q, want %q", imp.Func.Name, "log")
	}
	if len(imp.Func.Params) != 1 {
		t.Fatalf("expected 1 param, got %d", len(imp.Func.Params))
	}

	exp := w.Exports[0]
	if exp.Func == nil {
		t.Fatal("expected inline func export")
	}
	if exp.Func.Name != "run" {
		t.Errorf("export func name: got %q, want %q", exp.Func.Name, "run")
	}
	if exp.Func.Results == nil || exp.Func.Results.Anon == nil {
		t.Fatal("expected result on export func")
	}
}

func TestParseWorldWithInterfaceRef(t *testing.T) {
	src := `
world my-world {
    import wasi:filesystem/types@0.2.0;
    export wasi:http/handler;
}
`
	file, errs := Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	w := file.Worlds[0]
	if len(w.Imports) != 1 {
		t.Fatalf("expected 1 import, got %d", len(w.Imports))
	}
	if w.Imports[0].Interface != "wasi:filesystem/types@0.2.0" {
		t.Errorf("import interface: got %q", w.Imports[0].Interface)
	}
	if len(w.Exports) != 1 {
		t.Fatalf("expected 1 export, got %d", len(w.Exports))
	}
	if w.Exports[0].Interface != "wasi:http/handler" {
		t.Errorf("export interface: got %q", w.Exports[0].Interface)
	}
}

func TestParseNamedResults(t *testing.T) {
	src := `
interface api {
    get-pair: func() -> (first: u32, second: string);
}
`
	file, errs := Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	fn := file.Interfaces[0].Items[0].(*Func)
	if fn.Results == nil {
		t.Fatal("expected results")
	}
	if len(fn.Results.Named) != 2 {
		t.Fatalf("expected 2 named results, got %d", len(fn.Results.Named))
	}
	if fn.Results.Named[0].Name != "first" || fn.Results.Named[0].Type.Builtin != "u32" {
		t.Errorf("result 0: got %q:%q", fn.Results.Named[0].Name, fn.Results.Named[0].Type.Builtin)
	}
	if fn.Results.Named[1].Name != "second" || fn.Results.Named[1].Type.Builtin != "string" {
		t.Errorf("result 1: got %q:%q", fn.Results.Named[1].Name, fn.Results.Named[1].Type.Builtin)
	}
}

func TestParseResultNoTypeArgs(t *testing.T) {
	src := `
interface api {
    do-thing: func() -> result;
}
`
	file, errs := Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	fn := file.Interfaces[0].Items[0].(*Func)
	if fn.Results == nil || fn.Results.Anon == nil {
		t.Fatal("expected anon result")
	}
	if fn.Results.Anon.Kind != ResultType {
		t.Errorf("expected result type, got %v", fn.Results.Anon.Kind)
	}
	if fn.Results.Anon.Ok != nil {
		t.Error("expected no Ok type on bare result")
	}
}

func TestParseResultWithUnderscore(t *testing.T) {
	src := `
interface api {
    fail: func() -> result<_, string>;
    succeed: func() -> result<u32, _>;
}
`
	file, errs := Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	items := file.Interfaces[0].Items

	fn0 := items[0].(*Func)
	if fn0.Results.Anon.Ok != nil {
		t.Error("expected void ok (underscore)")
	}
	if fn0.Results.Anon.Err == nil || fn0.Results.Anon.Err.Builtin != "string" {
		t.Error("expected err=string")
	}

	fn1 := items[1].(*Func)
	if fn1.Results.Anon.Ok == nil || fn1.Results.Anon.Ok.Builtin != "u32" {
		t.Error("expected ok=u32")
	}
	if fn1.Results.Anon.Err != nil {
		t.Error("expected void err (underscore)")
	}
}

func TestParseAllPrimitiveBuiltins(t *testing.T) {
	src := `
interface types {
    f1: func(a: s8, b: s16, c: s32, d: s64);
    f2: func(a: f32, b: char);
}
`
	file, errs := Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	items := file.Interfaces[0].Items
	fn1 := items[0].(*Func)
	expected := []string{"s8", "s16", "s32", "s64"}
	for i, want := range expected {
		if fn1.Params[i].Type.Builtin != want {
			t.Errorf("param %d: got %q, want %q", i, fn1.Params[i].Type.Builtin, want)
		}
	}
	fn2 := items[1].(*Func)
	if fn2.Params[0].Type.Builtin != "f32" {
		t.Errorf("expected f32, got %q", fn2.Params[0].Type.Builtin)
	}
	if fn2.Params[1].Type.Builtin != "char" {
		t.Errorf("expected char, got %q", fn2.Params[1].Type.Builtin)
	}
}

func TestLexerBareDash(t *testing.T) {
	src := `- record`
	lex := NewLexer(src, "test.wit")

	tok1 := lex.Next()
	if tok1.Kind != TokenIdent || tok1.Value != "-" {
		t.Errorf("expected ident \"-\", got %s %q", tok1.Kind, tok1.Value)
	}
	tok2 := lex.Next()
	if tok2.Kind != TokenRecord {
		t.Errorf("expected record, got %s", tok2.Kind)
	}
}

func TestParseErrorRecovery(t *testing.T) {
	// Invalid token where a type is expected
	src := `
interface types {
    bad: func() -> @;
}
`
	_, errs := Parse(src, "test.wit")
	if len(errs) == 0 {
		t.Fatal("expected parse errors")
	}
}

func TestParseFullExample(t *testing.T) {
	src := `
package wasi:filesystem@0.2.0;

interface types {
    enum descriptor-type {
        unknown,
        directory,
        regular-file,
    }

    record descriptor-stat {
        type: descriptor-type,
        size: u64,
    }

    flags open-flags {
        create,
        exclusive,
        truncate,
    }

    resource descriptor {
        stat: func() -> result<descriptor-stat, u32>;
        read: func(length: u64, offset: u64) -> result<list<u8>, u32>;
        write: func(data: list<u8>, offset: u64) -> result<u64, u32>;
    }

    open-at: func(path: string, flags: open-flags) -> result<descriptor, u32>;
}
`
	file, errs := Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	if file.Package == nil {
		t.Fatal("expected package")
	}
	if len(file.Interfaces) != 1 {
		t.Fatalf("expected 1 interface, got %d", len(file.Interfaces))
	}
	iface := file.Interfaces[0]
	if iface.Name != "types" {
		t.Errorf("interface name: got %q", iface.Name)
	}
	// 3 type decls + 1 resource + 1 function = 5 items
	if len(iface.Items) != 5 {
		t.Errorf("expected 5 items, got %d", len(iface.Items))
	}
}
