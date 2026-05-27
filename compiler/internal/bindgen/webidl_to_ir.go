package bindgen

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"djabi.dev/go/promise_lang/internal/webidl"
)

// WebIdlToIR converts a parsed WebIDL file into a slice of binding IR modules.
// Each WebIDL interface produces one IR Resource. Dictionaries become Records.
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

func (c *webidlConverter) convertEnum(e *webidl.Enum) Type {
	t := Type{
		Name: e.Name,
		Kind: TypeEnum,
		Doc:  e.Doc,
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
		Doc:  d.Doc,
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
		Name: iface.Name,
		Drop: true,
		Doc:  iface.Doc,
	}

	for _, member := range iface.Members {
		switch m := member.(type) {
		case *webidl.Attribute:
			// Getter
			getter := Func{
				Name:       idlToSnake(m.Name),
				Kind:       FuncMethod,
				Accessor:   AccessorGetter,
				OwnerType:  iface.Name,
				ImportName: iface.Name + "." + m.Name + ".get",
				Results:    []TypeRef{convertWebIdlTypeRef(m.Type)},
				Doc:        m.Doc,
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
					ImportName: iface.Name + "." + m.Name + ".set",
					Params:     []Param{{Name: "value", Type: convertWebIdlTypeRef(m.Type)}},
					Doc:        m.Doc,
				}
				if m.Static {
					setter.Kind = FuncStatic
				}
				res.Methods = append(res.Methods, setter)
			}

		case *webidl.Operation:
			if m.Special == "getter" || m.Special == "setter" || m.Special == "deleter" {
				// Special operations — emit with special naming
				f := c.convertOperation(iface.Name, m)
				res.Methods = append(res.Methods, f)
			} else if m.Name != "" {
				f := c.convertOperation(iface.Name, m)
				res.Methods = append(res.Methods, f)
			}

		case *webidl.Constructor:
			f := c.convertConstructor(iface.Name, m)
			res.Methods = append(res.Methods, f)

		case *webidl.Const:
			// Constants become static getter-like functions
			f := Func{
				Name:       idlToSnake(m.Name),
				Kind:       FuncStatic,
				OwnerType:  iface.Name,
				ImportName: iface.Name + "." + m.Name,
				Results:    []TypeRef{convertWebIdlTypeRef(m.Type)},
			}
			res.Methods = append(res.Methods, f)
		}
	}

	return res
}

func (c *webidlConverter) convertOperation(ifaceName string, op *webidl.Operation) Func {
	name := idlToSnake(op.Name)
	if op.Special != "" && op.Name == "" {
		name = op.Special
	}

	f := Func{
		Name:       name,
		Kind:       FuncMethod,
		OwnerType:  ifaceName,
		ImportName: ifaceName + "." + op.Name,
		Doc:        op.Doc,
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
	return b.String()
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

// moduleHasJsValue returns true if any TypeRef in the module references JsValue.
func moduleHasJsValue(m *Module) bool {
	var check func(ref TypeRef) bool
	check = func(ref TypeRef) bool {
		if ref.Kind == NamedKind && ref.Name == "JsValue" {
			return true
		}
		if ref.Elem != nil && check(*ref.Elem) {
			return true
		}
		if ref.Ok != nil && check(*ref.Ok) {
			return true
		}
		if ref.Err != nil && check(*ref.Err) {
			return true
		}
		for _, e := range ref.Elements {
			if check(e) {
				return true
			}
		}
		return false
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
