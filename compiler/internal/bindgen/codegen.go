package bindgen

import (
	"fmt"
	"strings"
)

// GeneratePromise generates Promise source code (.pr) from binding IR modules.
// The target parameter controls the `target annotation (e.g. "wasi", "web").
func GeneratePromise(modules []*Module, target string) string {
	g := &generator{
		target: target,
	}
	for _, m := range modules {
		g.emitModule(m)
	}
	return g.buf.String()
}

type generator struct {
	buf    strings.Builder
	target string
	indent int
}

func (g *generator) line(format string, args ...interface{}) {
	for range g.indent {
		g.buf.WriteString("    ")
	}
	fmt.Fprintf(&g.buf, format, args...)
	g.buf.WriteByte('\n')
}

func (g *generator) blank() {
	g.buf.WriteByte('\n')
}

func (g *generator) emitModule(m *Module) {
	// Emit types
	for _, t := range m.Types {
		g.emitType(t, m.ImportModule)
		g.blank()
	}

	// Emit resources
	for _, r := range m.Resources {
		g.emitResource(r, m.ImportModule)
		g.blank()
	}

	// Emit free functions: private extern + public wrapper
	for _, f := range m.Functions {
		g.emitFreeFunc(f, m.ImportModule)
		g.blank()
	}
}

func (g *generator) emitType(t Type, importModule string) {
	switch t.Kind {
	case TypeRecord:
		g.emitRecord(t)
	case TypeEnum:
		g.emitEnum(t)
	case TypeVariant:
		g.emitVariant(t)
	case TypeFlags:
		g.emitFlags(t)
	case TypeAlias:
		// Type aliases are inlined at use sites in Promise.
		// Emit a comment for documentation.
		if t.Doc != "" {
			g.line("// %s: %s", t.Name, t.Doc)
		}
	}
}

func (g *generator) emitRecord(t Type) {
	if t.Doc != "" {
		g.line("`doc \"%s\"", escapeDoc(t.Doc))
	}
	g.line("type %s `public `value {", t.Name)
	g.indent++
	for _, f := range t.Fields {
		g.line("%s %s `value;", promiseType(f.Type), f.Name)
	}
	g.indent--
	g.line("}")
}

func (g *generator) emitEnum(t Type) {
	if t.Doc != "" {
		g.line("`doc \"%s\"", escapeDoc(t.Doc))
	}
	g.line("enum %s `public {", t.Name)
	g.indent++
	for _, c := range t.Cases {
		g.line("%s,", c.Name)
	}
	g.indent--
	g.line("}")
}

func (g *generator) emitVariant(t Type) {
	if t.Doc != "" {
		g.line("`doc \"%s\"", escapeDoc(t.Doc))
	}
	g.line("enum %s `public {", t.Name)
	g.indent++
	for _, c := range t.Cases {
		if c.Type != nil {
			g.line("%s(%s value),", c.Name, promiseType(*c.Type))
		} else {
			g.line("%s,", c.Name)
		}
	}
	g.indent--
	g.line("}")
}

func (g *generator) emitFlags(t Type) {
	if t.Doc != "" {
		g.line("`doc \"%s\"", escapeDoc(t.Doc))
	}
	g.line("type %s `public `value {", t.Name)
	g.indent++
	g.line("int _bits `value;")
	g.blank()
	// Named flag constants as static methods
	for i, f := range t.Fields {
		g.line("`doc \"Flag: %s\"", f.Name)
		g.line("get %s %s `public `static {", f.Name, t.Name)
		g.indent++
		g.line("return %s(_bits: %d);", t.Name, 1<<uint(i))
		g.indent--
		g.line("}")
		g.blank()
	}
	// has method
	g.line("`doc \"Check if a flag is set.\"")
	g.line("has(this, %s flag) bool `public {", t.Name)
	g.indent++
	g.line("return (this._bits & flag._bits) != 0;")
	g.indent--
	g.line("}")
	g.blank()
	// set method (returns new flags with flag set)
	g.line("`doc \"Return a copy with the given flag set.\"")
	g.line("set(this, %s flag) %s `public {", t.Name, t.Name)
	g.indent++
	g.line("return %s(_bits: this._bits | flag._bits);", t.Name)
	g.indent--
	g.line("}")
	g.indent--
	g.line("}")
}

func (g *generator) emitResource(r Resource, importModule string) {
	if r.Doc != "" {
		g.line("`doc \"%s\"", escapeDoc(r.Doc))
	}
	g.line("type %s `public {", r.Name)
	g.indent++
	g.line("int _handle;")
	g.blank()

	// Methods
	for _, m := range r.Methods {
		g.emitResourceMethod(m, r.Name, importModule)
		g.blank()
	}

	// Drop
	if r.Drop {
		g.line("drop(~this) {")
		g.indent++
		g.line("_%s_drop(this._handle);", toSnake(r.Name))
		g.indent--
		g.line("}")
	}

	g.indent--
	g.line("}")

	// Emit private extern declarations for resource methods
	for _, m := range r.Methods {
		g.blank()
		g.emitResourceMethodExtern(m, r.Name, importModule)
	}

	// Drop extern
	if r.Drop {
		g.blank()
		externName := fmt.Sprintf("_%s_drop", toSnake(r.Name))
		importName := "[resource-drop]" + toKebab(r.Name)
		g.line("%s(int handle)", externName)
		g.indent++
		g.line("`extern(\"%s_drop\")", toSnake(r.Name))
		g.line("`wasm_import(\"%s\", \"%s\")", importModule, importName)
		g.line("`target(%s);", g.target)
		g.indent--
	}
}

func (g *generator) emitResourceMethod(m Func, resourceName, importModule string) {
	if m.Doc != "" {
		g.line("`doc \"%s\"", escapeDoc(m.Doc))
	}

	switch m.Kind {
	case FuncConstructor:
		g.emitConstructorWrapper(m, resourceName, importModule)
	case FuncStatic:
		g.emitStaticWrapper(m, resourceName, importModule)
	default:
		g.emitMethodWrapper(m, resourceName, importModule)
	}
}

func (g *generator) emitConstructorWrapper(m Func, resourceName, importModule string) {
	params := g.formatParams(m.Params)
	externName := fmt.Sprintf("_%s_constructor", toSnake(resourceName))
	externParams := g.formatExternCallArgs(m.Params)

	if isFailable(m.Results) {
		g.line("create!(%s) %s `public `static {", params, resourceName)
		g.indent++
		g.line("handle := %s(%s)^;", externName, externParams)
		g.line("return %s(_handle: handle);", resourceName)
		g.indent--
	} else {
		g.line("create(%s) %s `public `static {", params, resourceName)
		g.indent++
		g.line("handle := %s(%s);", externName, externParams)
		g.line("return %s(_handle: handle);", resourceName)
		g.indent--
	}
	g.line("}")
}

func (g *generator) emitStaticWrapper(m Func, resourceName, importModule string) {
	params := g.formatParams(m.Params)
	externName := fmt.Sprintf("_%s_%s", toSnake(resourceName), m.Name)
	externParams := g.formatExternCallArgs(m.Params)
	retType := g.formatReturnType(m.Results)
	failable := isFailable(m.Results)

	failMark := ""
	raise := ""
	if failable {
		failMark = "!"
		raise = "^"
	}

	if retType != "" {
		g.line("%s%s(%s) %s `public `static {", m.Name, failMark, params, retType)
		g.indent++
		g.line("return %s(%s)%s;", externName, externParams, raise)
		g.indent--
	} else {
		g.line("%s%s(%s) `public `static {", m.Name, failMark, params)
		g.indent++
		g.line("%s(%s)%s;", externName, externParams, raise)
		g.indent--
	}
	g.line("}")
}

func (g *generator) emitMethodWrapper(m Func, resourceName, importModule string) {
	params := g.formatParams(m.Params)
	externName := fmt.Sprintf("_%s_%s", toSnake(resourceName), m.Name)
	retType := g.formatReturnType(m.Results)
	failable := isFailable(m.Results)

	failMark := ""
	raise := ""
	if failable {
		failMark = "!"
		raise = "^"
	}

	// Build extern call args: this._handle, then regular params
	var callArgs []string
	callArgs = append(callArgs, "this._handle")
	for _, p := range m.Params {
		callArgs = append(callArgs, p.Name)
	}
	externCallArgs := strings.Join(callArgs, ", ")

	thisParam := "this"
	if params != "" {
		thisParam = "this, " + params
	}

	if retType != "" {
		g.line("%s%s(%s) %s `public {", m.Name, failMark, thisParam, retType)
		g.indent++
		g.line("return %s(%s)%s;", externName, externCallArgs, raise)
		g.indent--
	} else {
		g.line("%s%s(%s) `public {", m.Name, failMark, thisParam)
		g.indent++
		g.line("%s(%s)%s;", externName, externCallArgs, raise)
		g.indent--
	}
	g.line("}")
}

func (g *generator) emitResourceMethodExtern(m Func, resourceName, importModule string) {
	switch m.Kind {
	case FuncConstructor:
		externName := fmt.Sprintf("_%s_constructor", toSnake(resourceName))
		params := g.formatExternParams(m.Params)
		retSig := "int"
		failable := isFailable(m.Results)
		if failable {
			retSig = "int!"
		}
		g.line("%s(%s) %s", externName, params, retSig)
	case FuncStatic:
		externName := fmt.Sprintf("_%s_%s", toSnake(resourceName), m.Name)
		params := g.formatExternParams(m.Params)
		retSig := g.formatReturnSig(m.Results)
		g.line("%s(%s)%s", externName, params, retSig)
	default:
		externName := fmt.Sprintf("_%s_%s", toSnake(resourceName), m.Name)
		// Method extern takes handle as first param
		handleParam := "int handle"
		extraParams := g.formatExternParams(m.Params)
		allParams := handleParam
		if extraParams != "" {
			allParams = handleParam + ", " + extraParams
		}
		retSig := g.formatReturnSig(m.Results)
		g.line("%s(%s)%s", externName, allParams, retSig)
	}
	g.indent++
	externLinkName := toSnake(resourceName) + "_" + m.Name
	if m.Kind == FuncConstructor {
		externLinkName = toSnake(resourceName) + "_constructor"
	}
	g.line("`extern(\"%s\")", externLinkName)
	g.line("`wasm_import(\"%s\", \"%s\")", importModule, m.ImportName)
	g.line("`target(%s);", g.target)
	g.indent--
}

func (g *generator) emitFreeFunc(f Func, importModule string) {
	externName := "_" + f.Name
	params := g.formatParams(f.Params)
	externParams := g.formatExternParams(f.Params)
	retType := g.formatReturnType(f.Results)
	retSig := g.formatReturnSig(f.Results)
	failable := isFailable(f.Results)
	externCallArgs := g.formatExternCallArgs(f.Params)

	failMark := ""
	raise := ""
	if failable {
		failMark = "!"
		raise = "^"
	}

	// Public wrapper
	if f.Doc != "" {
		g.line("`doc \"%s\"", escapeDoc(f.Doc))
	}
	if retType != "" {
		g.line("%s%s(%s) %s `public {", f.Name, failMark, params, retType)
		g.indent++
		g.line("return %s(%s)%s;", externName, externCallArgs, raise)
		g.indent--
	} else {
		g.line("%s%s(%s) `public {", f.Name, failMark, params)
		g.indent++
		g.line("%s(%s)%s;", externName, externCallArgs, raise)
		g.indent--
	}
	g.line("}")
	g.blank()

	// Private extern
	g.line("%s(%s)%s", externName, externParams, retSig)
	g.indent++
	g.line("`extern(\"%s\")", f.Name)
	g.line("`wasm_import(\"%s\", \"%s\")", importModule, f.ImportName)
	g.line("`target(%s);", g.target)
	g.indent--
}

// Helper functions for formatting

func (g *generator) formatParams(params []Param) string {
	var parts []string
	for _, p := range params {
		parts = append(parts, fmt.Sprintf("%s %s", promiseType(p.Type), p.Name))
	}
	return strings.Join(parts, ", ")
}

func (g *generator) formatExternParams(params []Param) string {
	// For now, same as formatParams — canonical ABI lowering is a future step
	return g.formatParams(params)
}

func (g *generator) formatExternCallArgs(params []Param) string {
	var parts []string
	for _, p := range params {
		parts = append(parts, p.Name)
	}
	return strings.Join(parts, ", ")
}

func (g *generator) formatReturnType(results []TypeRef) string {
	if len(results) == 0 {
		return ""
	}
	if len(results) == 1 {
		ref := results[0]
		if ref.Kind == ResultKind {
			if ref.Ok != nil {
				return promiseType(*ref.Ok)
			}
			return ""
		}
		return promiseType(ref)
	}
	// Multiple results → tuple
	var parts []string
	for _, r := range results {
		parts = append(parts, promiseType(r))
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

func (g *generator) formatReturnSig(results []TypeRef) string {
	retType := g.formatReturnType(results)
	failable := isFailable(results)
	if retType == "" && failable {
		return "!"
	}
	if retType == "" {
		return ""
	}
	if failable {
		return " " + retType + "!"
	}
	return " " + retType
}

// promiseType converts a binding IR TypeRef to a Promise type string.
func promiseType(ref TypeRef) string {
	switch ref.Kind {
	case BuiltinKind:
		return witBuiltinToPromise(ref.Builtin)
	case NamedKind:
		return ref.Name
	case ListKind:
		return promiseType(*ref.Elem) + "[]"
	case OptionKind:
		return promiseType(*ref.Elem) + "?"
	case ResultKind:
		// result<T, E> → T (failable handled at function level)
		if ref.Ok != nil {
			return promiseType(*ref.Ok)
		}
		return ""
	case TupleKind:
		var parts []string
		for _, e := range ref.Elements {
			parts = append(parts, promiseType(e))
		}
		return "(" + strings.Join(parts, ", ") + ")"
	case OwnKind:
		return "~" + promiseType(*ref.Elem)
	case BorrowKind:
		return "&" + promiseType(*ref.Elem)
	default:
		return "int"
	}
}

func witBuiltinToPromise(builtin string) string {
	switch builtin {
	case "u8":
		return "u8"
	case "u16":
		return "u16"
	case "u32":
		return "u32"
	case "u64":
		return "u64"
	case "s8":
		return "i8"
	case "s16":
		return "i16"
	case "s32":
		return "i32"
	case "s64":
		return "i64"
	case "f32":
		return "f32"
	case "f64":
		return "f64"
	case "bool":
		return "bool"
	case "char":
		return "char"
	case "string":
		return "string"
	default:
		return "int"
	}
}

func isFailable(results []TypeRef) bool {
	for _, r := range results {
		if r.Kind == ResultKind {
			return true
		}
	}
	return false
}

// toSnake converts PascalCase to snake_case
func toSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r + 32) // toLower
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// toKebab converts PascalCase to kebab-case
func toKebab(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r + 32) // toLower
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func escapeDoc(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}
