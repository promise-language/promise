package bindgen

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/promise-language/promise/compiler/internal/webidl"
)

// WebIdlToIR converts a parsed WebIDL file into a slice of binding IR modules.
// Each WebIDL interface produces one IR Resource, which records its inheritance
// parent but carries only its own members — the generated `is` clause is how a
// derived interface reaches its parent's (T1507). Dictionaries become Records.
// Enums become IR Enums. The file produces a single module named from the
// first interface, or "web" if no interfaces are present.
//
// Call webidl.Merge(file) before this function to apply partials, mixins, and
// includes statements.
func WebIdlToIR(file *webidl.File) []*Module {
	c := &webidlConverter{
		file: file,
	}
	return c.convert()
}

type webidlConverter struct {
	file *webidl.File
}

func (c *webidlConverter) convert() []*Module {
	m := &Module{
		Name:         "web",
		ImportModule: "promise_env",
	}

	// Convert enums
	for _, e := range c.file.Enums {
		m.Types = append(m.Types, c.convertEnum(e))
	}

	// Convert dictionaries as records
	for _, d := range c.file.Dictionaries {
		m.Types = append(m.Types, c.convertDictionary(d))
	}

	// Convert typedefs as aliases
	for _, td := range c.file.Typedefs {
		m.Types = append(m.Types, c.convertTypedef(td))
	}

	// Convert interfaces as resources
	for _, iface := range c.file.Interfaces {
		// Skip mixin-tagged interfaces (they're merged into targets already)
		if isMixin(iface) {
			continue
		}
		m.Resources = append(m.Resources, c.convertInterface(iface))
	}

	// Convert callbacks as function type aliases (skipped in IR for now —
	// callbacks would require lambda types which aren't in the IR)

	for _, iface := range c.file.Interfaces {
		if !isMixin(iface) {
			m.Name = idlToSnake(iface.Name)
			break
		}
	}

	// Scan all type references for JsValue usage
	m.HasJsValue = moduleHasJsValue(m)

	return []*Module{m}
}

func isMixin(iface *webidl.Interface) bool {
	for _, ea := range iface.ExtAttrs {
		if ea.Name == "_mixin" {
			return true
		}
	}
	return false
}

// webidlDoc returns the `doc text of a declaration bound from WebIDL: its IDL
// comment, or — spec IDL carries almost none, its prose living outside the IDL
// block — a line naming the construct it binds, e.g. "`Element.setAttribute`
// (WebIDL operation)" (docs/wasm-bindings.md, "Generated Module Contents").
func webidlDoc(comment, construct, idlName string) string {
	if comment != "" {
		return comment
	}
	return "`" + idlName + "` (WebIDL " + construct + ")"
}

func (c *webidlConverter) convertEnum(e *webidl.Enum) Type {
	t := Type{
		Name: e.Name,
		Kind: TypeEnum,
		Doc:  webidlDoc(e.Doc, "enum", e.Name),
	}
	for _, v := range e.Values {
		t.Cases = append(t.Cases, Case{
			Name: idlEnumValueToPascal(v),
		})
	}
	return t
}

func (c *webidlConverter) convertDictionary(d *webidl.Dictionary) Type {
	t := Type{
		Name: d.Name,
		Kind: TypeRecord,
		Doc:  webidlDoc(d.Doc, "dictionary", d.Name),
	}
	for _, m := range d.Members {
		f := Field{
			Name: idlToSnake(m.Name),
			Type: convertWebIdlTypeRef(m.Type),
		}
		if !m.Required {
			// Optional dictionary members map to Option type
			elem := f.Type
			f.Type = TypeRef{Kind: OptionKind, Elem: &elem}
		}
		t.Fields = append(t.Fields, f)
	}
	return t
}

func (c *webidlConverter) convertTypedef(td *webidl.Typedef) Type {
	ref := convertWebIdlTypeRef(td.Type)
	return Type{
		Name:   td.Name,
		Kind:   TypeAlias,
		Target: &ref,
		Doc:    td.Doc,
	}
}

func (c *webidlConverter) convertInterface(iface *webidl.Interface) Resource {
	res := Resource{
		Name:   iface.Name,
		Parent: iface.Parent,
		Drop:   true,
		Doc:    webidlDoc(iface.Doc, "interface", iface.Name),
	}

	// How many operations of each Promise name this interface has emitted so
	// far, in merged member order — see numberOverload.
	overloads := make(map[string]int)

	for _, member := range iface.Members {
		switch m := member.(type) {
		case *webidl.Attribute:
			idlName := iface.Name + "." + m.Name
			doc := webidlDoc(m.Doc, "attribute", idlName)
			// Getter
			getter := Func{
				Name:       idlToSnake(m.Name),
				Kind:       FuncMethod,
				Accessor:   AccessorGetter,
				OwnerType:  iface.Name,
				ImportName: idlName + ".get",
				Results:    []TypeRef{convertWebIdlTypeRef(m.Type)},
				Doc:        doc,
			}
			if m.Static {
				getter.Kind = FuncStatic
			}
			res.Methods = append(res.Methods, getter)

			// Setter (unless readonly)
			if !m.Readonly {
				setter := Func{
					Name:       idlToSnake(m.Name),
					Kind:       FuncMethod,
					Accessor:   AccessorSetter,
					OwnerType:  iface.Name,
					ImportName: idlName + ".set",
					Params:     []Param{{Name: "value", Type: convertWebIdlTypeRef(m.Type)}},
					Doc:        doc,
				}
				if m.Static {
					setter.Kind = FuncStatic
				}
				res.Methods = append(res.Methods, setter)
			}

		case *webidl.Operation:
			// Named operations, and the anonymous special operations (emitted
			// under their special keyword). Anything else without a name — an
			// anonymous stringifier or legacycaller — has no member to emit.
			special := m.Special == "getter" || m.Special == "setter" || m.Special == "deleter"
			if m.Name == "" && !special {
				continue
			}
			f := c.convertOperation(iface.Name, m)
			overloads[f.Name]++
			numberOverload(&f, overloads[f.Name])
			res.Methods = append(res.Methods, f)

		case *webidl.Constructor:
			f := c.convertConstructor(iface.Name, m)
			res.Methods = append(res.Methods, f)

		case *webidl.Const:
			// Constants become static getter-like functions
			idlName := iface.Name + "." + m.Name
			f := Func{
				Name:       idlToSnake(m.Name),
				Kind:       FuncStatic,
				OwnerType:  iface.Name,
				ImportName: idlName,
				Results:    []TypeRef{convertWebIdlTypeRef(m.Type)},
				Doc:        webidlDoc(m.Doc, "constant", idlName),
			}
			res.Methods = append(res.Methods, f)
		}
	}

	return res
}

// numberOverload names the k-th operation of an interface's overload set (k is
// 1-based, in merged declaration order). Promise has no overloading
// (docs/language-design.md, "No Overloading"), so every overload after the first
// becomes a member of its own: `<name>_<k>`, imported as `<Interface>.<member>.<k>`.
// The import keeps the IDL member as its second component, so the JS glue calls
// that same member with this overload's own arguments and the host's overload
// resolution chooses among them (docs/wasm-bindings.md, "Overloaded Operations").
// The extern and link names derive from f.Name, so they follow automatically.
func numberOverload(f *Func, k int) {
	if k < 2 {
		return
	}
	f.Name = fmt.Sprintf("%s_%d", f.Name, k)
	f.ImportName = fmt.Sprintf("%s.%d", f.ImportName, k)
}

func (c *webidlConverter) convertOperation(ifaceName string, op *webidl.Operation) Func {
	name := idlToSnake(op.Name)
	// The import names the JS member the glue calls. An anonymous special
	// operation has no identifier, so it is named by its special keyword on
	// both sides — never by an empty member, which would emit `obj.(...)`.
	member := op.Name
	if op.Special != "" && op.Name == "" {
		name = op.Special
		member = op.Special
	}

	idlName := ifaceName + "." + member
	f := Func{
		Name:       name,
		Kind:       FuncMethod,
		OwnerType:  ifaceName,
		ImportName: idlName,
		Doc:        webidlDoc(op.Doc, "operation", idlName),
	}
	if op.Static {
		f.Kind = FuncStatic
	}

	for _, p := range op.Params {
		param := Param{
			Name: idlToSnake(p.Name),
			Type: convertWebIdlTypeRef(p.Type),
		}
		if p.Optional {
			elem := param.Type
			param.Type = TypeRef{Kind: OptionKind, Elem: &elem}
		}
		f.Params = append(f.Params, param)
	}

	if op.Return != nil && op.Return.Builtin != "void" && op.Return.Builtin != "undefined" {
		f.Results = []TypeRef{convertWebIdlTypeRef(op.Return)}
	}

	return f
}

func (c *webidlConverter) convertConstructor(ifaceName string, ctor *webidl.Constructor) Func {
	f := Func{
		Name:       "create",
		Kind:       FuncConstructor,
		OwnerType:  ifaceName,
		ImportName: ifaceName + ".constructor",
		Doc:        ctor.Doc,
	}
	for _, p := range ctor.Params {
		param := Param{
			Name: idlToSnake(p.Name),
			Type: convertWebIdlTypeRef(p.Type),
		}
		if p.Optional {
			elem := param.Type
			param.Type = TypeRef{Kind: OptionKind, Elem: &elem}
		}
		f.Params = append(f.Params, param)
	}
	return f
}

// convertWebIdlTypeRef converts a WebIDL type reference to the binding IR TypeRef.
func convertWebIdlTypeRef(ref *webidl.TypeRef) TypeRef {
	if ref == nil {
		return TypeRef{Kind: BuiltinKind, Builtin: "void"}
	}

	var result TypeRef

	switch ref.Kind {
	case webidl.BuiltinType:
		result = convertWebIdlBuiltin(ref.Builtin)
	case webidl.NamedType:
		result = TypeRef{Kind: NamedKind, Name: ref.Name}
	case webidl.SequenceType:
		elem := convertWebIdlTypeRef(ref.Elem)
		result = TypeRef{Kind: ListKind, Elem: &elem}
	case webidl.FrozenArrayType:
		elem := convertWebIdlTypeRef(ref.Elem)
		result = TypeRef{Kind: ListKind, Elem: &elem}
	case webidl.ObservableArrayType:
		elem := convertWebIdlTypeRef(ref.Elem)
		result = TypeRef{Kind: ListKind, Elem: &elem}
	case webidl.PromiseType:
		// Promise<T> → Named "JsValue" for now (tasks are separate)
		result = TypeRef{Kind: NamedKind, Name: "JsValue"}
	case webidl.RecordType:
		// record<K, V> → Named "JsValue"
		result = TypeRef{Kind: NamedKind, Name: "JsValue"}
	case webidl.UnionType:
		// Union types → Named "JsValue" (most general)
		result = TypeRef{Kind: NamedKind, Name: "JsValue"}
	default:
		result = TypeRef{Kind: BuiltinKind, Builtin: "u32"}
	}

	// Handle nullable: T? → option<T>
	if ref.Nullable {
		elem := result
		result = TypeRef{Kind: OptionKind, Elem: &elem}
	}

	return result
}

func convertWebIdlBuiltin(builtin string) TypeRef {
	switch builtin {
	case "DOMString", "USVString", "ByteString":
		return TypeRef{Kind: BuiltinKind, Builtin: "string"}
	case "boolean":
		return TypeRef{Kind: BuiltinKind, Builtin: "bool"}
	case "byte":
		return TypeRef{Kind: BuiltinKind, Builtin: "s8"}
	case "octet":
		return TypeRef{Kind: BuiltinKind, Builtin: "u8"}
	case "short":
		return TypeRef{Kind: BuiltinKind, Builtin: "s16"}
	case "unsigned short":
		return TypeRef{Kind: BuiltinKind, Builtin: "u16"}
	case "long":
		return TypeRef{Kind: BuiltinKind, Builtin: "s32"}
	case "unsigned long":
		return TypeRef{Kind: BuiltinKind, Builtin: "u32"}
	case "long long":
		return TypeRef{Kind: BuiltinKind, Builtin: "s64"}
	case "unsigned long long":
		return TypeRef{Kind: BuiltinKind, Builtin: "u64"}
	case "float", "unrestricted float":
		return TypeRef{Kind: BuiltinKind, Builtin: "f32"}
	case "double", "unrestricted double":
		return TypeRef{Kind: BuiltinKind, Builtin: "f64"}
	case "void", "undefined":
		return TypeRef{Kind: BuiltinKind, Builtin: "void"}
	case "any", "object":
		return TypeRef{Kind: NamedKind, Name: "JsValue"}
	case "bigint":
		return TypeRef{Kind: BuiltinKind, Builtin: "s64"}
	case "symbol":
		return TypeRef{Kind: NamedKind, Name: "JsValue"}
	case "ArrayBuffer", "DataView",
		"Int8Array", "Int16Array", "Int32Array",
		"Uint8Array", "Uint16Array", "Uint32Array", "Uint8ClampedArray",
		"Float32Array", "Float64Array":
		return TypeRef{Kind: NamedKind, Name: "JsValue"}
	default:
		return TypeRef{Kind: BuiltinKind, Builtin: "u32"}
	}
}

// Name conversion utilities: WebIDL camelCase → Promise snake_case

// idlToSnake converts "getElementById" → "get_element_by_id"
func idlToSnake(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	prev := rune(0)
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			// Insert underscore before uppercase if:
			// 1. Not at start, AND
			// 2. Previous char was lowercase, OR
			// 3. Next char is lowercase (handles "getURL" → "get_url")
			if i > 0 && (unicode.IsLower(prev) || (i+1 < len(s) && unicode.IsLower(rune(s[i+1])))) {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteRune(r)
		}
		prev = r
	}
	return sanitizeIdent(b.String())
}

// promiseKeywords are reserved words that cannot appear as identifiers in
// generated Promise code. WebIDL freely uses some of these as parameter, member,
// and operation names (e.g. `type`), which would otherwise emit verbatim and fail
// to parse. sanitizeIdent rewrites any colliding identifier (T0724).
// Keep in sync with the keyword tokens in compiler/grammar/PromiseLexer.g4.
// `get`/`set` are contextual (getter/setter syntax) but are sanitized too to avoid
// ambiguity with a method/field of that name.
var promiseKeywords = map[string]bool{
	"absent": true, "as": true, "break": true, "continue": true, "else": true,
	"enum": true, "false": true, "for": true, "go": true, "if": true,
	"in": true, "is": true, "match": true, "move": true, "none": true,
	"present": true, "raise": true, "return": true, "select": true, "this": true,
	"true": true, "type": true, "use": true, "while": true,
	"yield": true, "get": true, "set": true,
}

// sanitizeIdent rewrites an identifier that collides with a Promise keyword by
// prefixing it with "_" (e.g. `type` -> `_type`). Applied inside idlToSnake so it
// covers every generated identifier position (params, dictionary fields,
// attribute/operation names). Type names stay PascalCase and never collide; extern
// link names and WASM import names derive from the raw IDL / PascalCase names, so
// wrapper, extern, and JS glue stay consistent.
func sanitizeIdent(s string) string {
	if promiseKeywords[s] {
		return "_" + s
	}
	return s
}

// idlEnumValueToPascal converts WebIDL string enum values to PascalCase.
// "read-write" → "ReadWrite", "auto" → "Auto"
func idlEnumValueToPascal(s string) string {
	// Split on hyphens and underscores
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == '-' || r == '_' || r == ' '
	})
	var b strings.Builder
	for _, part := range parts {
		if len(part) == 0 {
			continue
		}
		r, size := utf8.DecodeRuneInString(part)
		b.WriteRune(unicode.ToUpper(r))
		b.WriteString(part[size:])
	}
	result := b.String()
	if result == "" {
		return "Empty"
	}
	return result
}

// eachName calls fn with every NamedKind name in ref, depth-first, stopping at
// the first call that returns true; it reports whether one did. It is the one
// walk over a TypeRef's shape — a new TypeRefKind that can name a type extends
// it here.
func eachName(ref TypeRef, fn func(name string) bool) bool {
	if ref.Kind == NamedKind && fn(ref.Name) {
		return true
	}
	if ref.Elem != nil && eachName(*ref.Elem, fn) {
		return true
	}
	if ref.Ok != nil && eachName(*ref.Ok, fn) {
		return true
	}
	if ref.Err != nil && eachName(*ref.Err, fn) {
		return true
	}
	for _, e := range ref.Elements {
		if eachName(e, fn) {
			return true
		}
	}
	return false
}

// moduleHasJsValue returns true if any TypeRef in the module references JsValue.
func moduleHasJsValue(m *Module) bool {
	check := func(ref TypeRef) bool {
		return eachName(ref, func(name string) bool { return name == "JsValue" })
	}

	for _, t := range m.Types {
		for _, f := range t.Fields {
			if check(f.Type) {
				return true
			}
		}
		for _, c := range t.Cases {
			if c.Type != nil && check(*c.Type) {
				return true
			}
		}
		if t.Target != nil && check(*t.Target) {
			return true
		}
	}
	for _, f := range m.Functions {
		for _, p := range f.Params {
			if check(p.Type) {
				return true
			}
		}
		for _, r := range f.Results {
			if check(r) {
				return true
			}
		}
	}
	for _, r := range m.Resources {
		for _, method := range r.Methods {
			for _, p := range method.Params {
				if check(p.Type) {
					return true
				}
			}
			for _, res := range method.Results {
				if check(res) {
					return true
				}
			}
		}
	}
	return false
}
