package bindgen

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"djabi.dev/go/promise_lang/internal/wit"
)

// WitToIR converts a parsed WIT file into a slice of binding IR modules.
// Each WIT interface produces one IR Module.
func WitToIR(file *wit.File) []*Module {
	c := &witConverter{
		pkg:              file.Package,
		typesByInterface: make(map[string]map[string]Type),
	}
	// Pass 1: convert all interfaces, building the type registry.
	var modules []*Module
	for _, iface := range file.Interfaces {
		modules = append(modules, c.convertInterface(iface))
	}
	// Pass 2: resolve cross-interface use statements.
	for i, iface := range file.Interfaces {
		c.resolveUses(iface, modules[i])
	}
	return modules
}

type witConverter struct {
	pkg              *wit.Package
	typesByInterface map[string]map[string]Type // interfaceName → PascalName → Type
}

func (c *witConverter) importModule(ifaceName string) string {
	if c.pkg != nil {
		return c.pkg.Namespace + ":" + c.pkg.Name + "/" + ifaceName
	}
	return ifaceName
}

func (c *witConverter) convertInterface(iface *wit.Interface) *Module {
	m := &Module{
		Name:         kebabToSnake(iface.Name),
		ImportModule: c.importModule(iface.Name),
	}

	for _, item := range iface.Items {
		switch v := item.(type) {
		case *wit.Record:
			t := c.convertRecord(v)
			c.registerType(iface.Name, t)
			m.Types = append(m.Types, t)
		case *wit.Enum:
			t := c.convertEnum(v)
			c.registerType(iface.Name, t)
			m.Types = append(m.Types, t)
		case *wit.Variant:
			t := c.convertVariant(v)
			c.registerType(iface.Name, t)
			m.Types = append(m.Types, t)
		case *wit.Flags:
			t := c.convertFlags(v)
			c.registerType(iface.Name, t)
			m.Types = append(m.Types, t)
		case *wit.TypeAlias:
			t := c.convertTypeAlias(v)
			c.registerType(iface.Name, t)
			m.Types = append(m.Types, t)
		case *wit.Resource:
			m.Resources = append(m.Resources, c.convertResource(v, m.ImportModule))
		case *wit.Func:
			m.Functions = append(m.Functions, c.convertFunc(v, m.ImportModule))
		case *wit.Use:
			// Resolved in pass 2 by resolveUses.
		}
	}

	return m
}

// registerType adds a converted type to the per-interface registry.
func (c *witConverter) registerType(ifaceName string, t Type) {
	m := c.typesByInterface[ifaceName]
	if m == nil {
		m = make(map[string]Type)
		c.typesByInterface[ifaceName] = m
	}
	m[t.Name] = t
}

// resolveUses copies type definitions from source interfaces into importing
// modules for each use-statement. Only local (same-file) interfaces are
// resolved; external package references are silently skipped.
func (c *witConverter) resolveUses(iface *wit.Interface, m *Module) {
	for _, item := range iface.Items {
		u, ok := item.(*wit.Use)
		if !ok {
			continue
		}
		srcTypes := c.typesByInterface[u.Path]
		if srcTypes == nil {
			continue // external package reference, skip
		}
		for _, name := range u.Names {
			pascalName := kebabToPascal(name.Name)
			srcType, ok := srcTypes[pascalName]
			if !ok {
				continue
			}
			if name.As != "" {
				srcType.Name = kebabToPascal(name.As)
			}
			m.Types = append(m.Types, srcType)
		}
	}
}

func (c *witConverter) convertRecord(r *wit.Record) Type {
	t := Type{
		Name: kebabToPascal(r.Name),
		Kind: TypeRecord,
		Doc:  r.Doc,
	}
	for _, f := range r.Fields {
		t.Fields = append(t.Fields, Field{
			Name: kebabToSnake(f.Name),
			Type: convertTypeRef(f.Type),
		})
	}
	return t
}

func (c *witConverter) convertEnum(e *wit.Enum) Type {
	t := Type{
		Name: kebabToPascal(e.Name),
		Kind: TypeEnum,
		Doc:  e.Doc,
	}
	for _, caseName := range e.Cases {
		t.Cases = append(t.Cases, Case{
			Name: kebabToPascal(caseName),
		})
	}
	return t
}

func (c *witConverter) convertVariant(v *wit.Variant) Type {
	t := Type{
		Name: kebabToPascal(v.Name),
		Kind: TypeVariant,
		Doc:  v.Doc,
	}
	for _, vc := range v.Cases {
		irCase := Case{Name: kebabToPascal(vc.Name)}
		if vc.Type != nil {
			ref := convertTypeRef(vc.Type)
			irCase.Type = &ref
		}
		t.Cases = append(t.Cases, irCase)
	}
	return t
}

func (c *witConverter) convertFlags(f *wit.Flags) Type {
	t := Type{
		Name: kebabToPascal(f.Name),
		Kind: TypeFlags,
		Doc:  f.Doc,
	}
	for _, flagName := range f.Flags {
		t.Fields = append(t.Fields, Field{
			Name: kebabToSnake(flagName),
		})
	}
	return t
}

func (c *witConverter) convertTypeAlias(a *wit.TypeAlias) Type {
	ref := convertTypeRef(a.Target)
	return Type{
		Name:   kebabToPascal(a.Name),
		Kind:   TypeAlias,
		Target: &ref,
		Doc:    a.Doc,
	}
}

func (c *witConverter) convertResource(r *wit.Resource, importModule string) Resource {
	res := Resource{
		Name: kebabToPascal(r.Name),
		Drop: true,
		Doc:  r.Doc,
	}
	for _, m := range r.Methods {
		f := c.convertResourceMethod(m, r.Name, importModule)
		res.Methods = append(res.Methods, f)
	}
	return res
}

func (c *witConverter) convertResourceMethod(fn *wit.Func, resourceName, importModule string) Func {
	f := Func{
		Name:      kebabToSnake(fn.Name),
		OwnerType: kebabToPascal(resourceName),
		Doc:       fn.Doc,
	}

	switch fn.Kind {
	case wit.FuncConstructor:
		f.Kind = FuncConstructor
		f.ImportName = "[constructor]" + resourceName
	case wit.FuncStatic:
		f.Kind = FuncStatic
		f.ImportName = "[static]" + resourceName + "." + fn.Name
	default:
		f.Kind = FuncMethod
		f.ImportName = "[method]" + resourceName + "." + fn.Name
	}

	for _, p := range fn.Params {
		f.Params = append(f.Params, Param{
			Name: kebabToSnake(p.Name),
			Type: convertTypeRef(p.Type),
		})
	}
	if fn.Results != nil {
		f.Results = convertResults(fn.Results)
	}
	return f
}

func (c *witConverter) convertFunc(fn *wit.Func, importModule string) Func {
	f := Func{
		Name:       kebabToSnake(fn.Name),
		Kind:       FuncFree,
		ImportName: fn.Name,
		Doc:        fn.Doc,
	}
	for _, p := range fn.Params {
		f.Params = append(f.Params, Param{
			Name: kebabToSnake(p.Name),
			Type: convertTypeRef(p.Type),
		})
	}
	if fn.Results != nil {
		f.Results = convertResults(fn.Results)
	}
	return f
}

func convertResults(r *wit.Results) []TypeRef {
	if r.Anon != nil {
		return []TypeRef{convertTypeRef(r.Anon)}
	}
	var refs []TypeRef
	for _, p := range r.Named {
		refs = append(refs, convertTypeRef(p.Type))
	}
	return refs
}

func convertTypeRef(ref *wit.TypeRef) TypeRef {
	if ref == nil {
		return TypeRef{Kind: BuiltinKind, Builtin: "void"}
	}
	switch ref.Kind {
	case wit.BuiltinType:
		return TypeRef{Kind: BuiltinKind, Builtin: ref.Builtin}
	case wit.NamedType:
		return TypeRef{Kind: NamedKind, Name: kebabToPascal(ref.Name)}
	case wit.ListType:
		elem := convertTypeRef(ref.Elem)
		return TypeRef{Kind: ListKind, Elem: &elem}
	case wit.OptionType:
		elem := convertTypeRef(ref.Elem)
		return TypeRef{Kind: OptionKind, Elem: &elem}
	case wit.ResultType:
		r := TypeRef{Kind: ResultKind}
		if ref.Ok != nil {
			ok := convertTypeRef(ref.Ok)
			r.Ok = &ok
		}
		if ref.Err != nil {
			err := convertTypeRef(ref.Err)
			r.Err = &err
		}
		return r
	case wit.TupleType:
		r := TypeRef{Kind: TupleKind}
		for _, e := range ref.Elements {
			r.Elements = append(r.Elements, convertTypeRef(e))
		}
		return r
	case wit.OwnType:
		elem := convertTypeRef(ref.Elem)
		return TypeRef{Kind: OwnKind, Elem: &elem}
	case wit.BorrowType:
		elem := convertTypeRef(ref.Elem)
		return TypeRef{Kind: BorrowKind, Elem: &elem}
	default:
		return TypeRef{Kind: BuiltinKind, Builtin: "u32"}
	}
}

// Name conversion utilities: WIT kebab-case → Promise snake_case / PascalCase

// kebabToSnake converts "my-function-name" → "my_function_name"
func kebabToSnake(s string) string {
	return strings.ReplaceAll(s, "-", "_")
}

// kebabToPascal converts "my-type-name" → "MyTypeName"
func kebabToPascal(s string) string {
	parts := strings.Split(s, "-")
	var b strings.Builder
	for _, part := range parts {
		if len(part) == 0 {
			continue
		}
		r, size := utf8.DecodeRuneInString(part)
		b.WriteRune(unicode.ToUpper(r))
		b.WriteString(part[size:])
	}
	return b.String()
}
