package bindgen

import (
	"fmt"
	"strings"
)

// GeneratePromise generates Promise source code (.pr) from binding IR modules.
// The target parameter controls the `target annotation (e.g. "wasi", "web").
func GeneratePromise(modules []*Module, target string) string {
	return GeneratePromiseWithOptions(modules, target, false)
}

// GeneratePromiseWithOptions generates Promise source code with optional canonical ABI lowering.
// When canonicalABI is true, extern declarations use flattened canonical ABI types
// (i32/i64/f32/f64) and wrapper functions include marshalling code.
func GeneratePromiseWithOptions(modules []*Module, target string, canonicalABI bool) string {
	g := &generator{
		target:       target,
		canonicalABI: canonicalABI,
	}
	if canonicalABI {
		listElemTypes := collectListElemTypes(modules)
		g.emitCanonicalABIHelpers(listElemTypes)
	}
	for _, m := range modules {
		g.emitModule(m)
	}
	return g.buf.String()
}

type generator struct {
	buf          strings.Builder
	target       string
	indent       int
	canonicalABI bool
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
	// Emit JsValue enum if any type references it
	if m.HasJsValue {
		g.emitJsValueEnum()
		g.blank()
	}

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

// emitJsValueEnum emits the JsValue enum type used to represent dynamic
// JavaScript values (any, object, Promise<T>, record<K,V>, unions, etc.).
func (g *generator) emitJsValueEnum() {
	g.line("`doc \"Represents a dynamic JavaScript value.\"")
	g.line("enum JsValue `public {")
	g.indent++
	g.line("Undefined,")
	g.line("Null,")
	g.line("Bool(bool value),")
	g.line("Number(f64 value),")
	g.line("Str(string value),")
	g.line("Object(int _js_ref),")
	g.line("Array(int _js_ref),")
	g.line("Function(int _js_ref),")
	g.blank()
	g.line("`doc \"Returns true if this value is undefined.\"")
	g.line("get is_undefined bool `public {")
	g.indent++
	g.line("return this is Undefined;")
	g.indent--
	g.line("}")
	g.blank()
	g.line("`doc \"Returns true if this value is null.\"")
	g.line("get is_null bool `public {")
	g.indent++
	g.line("return this is Null;")
	g.indent--
	g.line("}")
	g.blank()
	g.line("`doc \"Returns the bool value, or none if not a Bool.\"")
	g.line("as_bool(this) bool? `public {")
	g.indent++
	g.line("return match this { Bool(v) => v, _ => none };")
	g.indent--
	g.line("}")
	g.blank()
	g.line("`doc \"Returns the number value, or none if not a Number.\"")
	g.line("as_number(this) f64? `public {")
	g.indent++
	g.line("return match this { Number(v) => v, _ => none };")
	g.indent--
	g.line("}")
	g.blank()
	g.line("`doc \"Returns the string value, or none if not a Str.\"")
	g.line("as_string(this) string? `public {")
	g.indent++
	g.line("return match this { Str(v) => v, _ => none };")
	g.indent--
	g.line("}")
	g.indent--
	g.line("}")
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
	g.line("type %s `public `target(%s) {", r.Name, g.target)
	g.indent++
	g.line("i32 _handle;")
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
		g.line("%s(i32 handle)", externName)
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

	// Check for resource return types
	resReturn, isResReturn := resourceReturnType(m.Results)
	optResReturn, isOptResReturn := optionResourceReturnType(m.Results)

	if !g.canonicalABI && isResReturn {
		g.line("%s%s(%s) %s `public `static {", m.Name, failMark, params, retType)
		g.indent++
		g.line("handle := %s(%s)%s;", externName, externParams, raise)
		g.line("return %s(_handle: handle);", resReturn)
		g.indent--
	} else if !g.canonicalABI && isOptResReturn {
		g.line("%s%s(%s) %s `public `static {", m.Name, failMark, params, retType)
		g.indent++
		g.line("handle := %s(%s)%s;", externName, externParams, raise)
		g.line("if handle == 0 { return none; }")
		g.line("return %s(_handle: handle);", optResReturn)
		g.indent--
	} else if retType != "" {
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

	// Build extern call args: this._handle, then params (._handle for resources)
	var callArgs []string
	callArgs = append(callArgs, "this._handle")
	for _, p := range m.Params {
		if !g.canonicalABI && isRefType(p.Type) {
			callArgs = append(callArgs, p.Name+"._handle")
		} else {
			callArgs = append(callArgs, p.Name)
		}
	}
	externCallArgs := strings.Join(callArgs, ", ")

	thisParam := "this"
	if params != "" {
		thisParam = "this, " + params
	}

	// Check for resource return types (need handle→resource construction)
	resReturn, isResReturn := resourceReturnType(m.Results)
	optResReturn, isOptResReturn := optionResourceReturnType(m.Results)

	if !g.canonicalABI && isResReturn {
		g.line("%s%s(%s) %s `public {", m.Name, failMark, thisParam, retType)
		g.indent++
		g.line("handle := %s(%s)%s;", externName, externCallArgs, raise)
		g.line("return %s(_handle: handle);", resReturn)
		g.indent--
	} else if !g.canonicalABI && isOptResReturn {
		g.line("%s%s(%s) %s `public {", m.Name, failMark, thisParam, retType)
		g.indent++
		g.line("handle := %s(%s)%s;", externName, externCallArgs, raise)
		g.line("if handle == 0 { return none; }")
		g.line("return %s(_handle: handle);", optResReturn)
		g.indent--
	} else if retType != "" {
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
		params := g.formatExternParamsWithRetPtr(m.Params, m.Results)
		retSig := "i32"
		failable := isFailable(m.Results)
		if failable {
			retSig = "i32!"
		}
		g.line("%s(%s) %s", externName, params, retSig)
	case FuncStatic:
		externName := fmt.Sprintf("_%s_%s", toSnake(resourceName), m.Name)
		params := g.formatExternParamsWithRetPtr(m.Params, m.Results)
		retSig := g.formatExternReturnSig(m.Results)
		g.line("%s(%s)%s", externName, params, retSig)
	default:
		externName := fmt.Sprintf("_%s_%s", toSnake(resourceName), m.Name)
		// Method extern takes handle (i32) as first param — handles are
		// 32-bit ref table indices on all WASM targets.
		handleParam := "i32 handle"
		extraParams := g.formatExternParamsWithRetPtr(m.Params, m.Results)
		allParams := handleParam
		if extraParams != "" {
			allParams = handleParam + ", " + extraParams
		}
		retSig := g.formatExternReturnSig(m.Results)
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
	retType := g.formatReturnType(f.Results)
	failable := isFailable(f.Results)

	failMark := ""
	raise := ""
	if failable {
		failMark = "!"
		raise = "^"
	}

	// Canonical ABI: check if retptr pattern is needed
	_, useRetPtr := flattenResults(f.Results)
	useRetPtr = useRetPtr && g.canonicalABI

	// Extern params (with retptr appended if needed)
	externParams := g.formatExternParamsWithRetPtr(f.Params, f.Results)
	externRetSig := g.formatExternReturnSig(f.Results)

	// Extern call args (with lowering + retptr)
	externCallArgs := g.formatCanonicalCallArgs(f.Params, useRetPtr)

	// Public wrapper
	if f.Doc != "" {
		g.line("`doc \"%s\"", escapeDoc(f.Doc))
	}
	if retType != "" {
		g.line("%s%s(%s) %s `public `target(%s) {", f.Name, failMark, params, retType, g.target)
		g.indent++
		if useRetPtr && g.canonicalABI {
			// Call extern (writes to retarea), then lift result
			g.line("%s(%s);", externName, externCallArgs)
			lifted := g.liftReturnFromRetPtr(f.Results)
			if failable {
				// Check discriminant for result<T, E>
				g.line("tag := _cabi_load_i32(_cabi_retarea_ptr());")
				errMsg := g.liftErrFromRetPtr(f.Results[0])
				g.line("if tag != 0 { raise error(%s); }", errMsg)
				g.line("return %s;", lifted)
			} else {
				g.line("return %s;", lifted)
			}
		} else {
			g.line("return %s(%s)%s;", externName, externCallArgs, raise)
		}
		g.indent--
	} else {
		g.line("%s%s(%s) `public `target(%s) {", f.Name, failMark, params, g.target)
		g.indent++
		if useRetPtr && g.canonicalABI && failable {
			g.line("%s(%s);", externName, externCallArgs)
			g.line("tag := _cabi_load_i32(_cabi_retarea_ptr());")
			errMsg := g.liftErrFromRetPtr(f.Results[0])
			g.line("if tag != 0 { raise error(%s); }", errMsg)
		} else {
			g.line("%s(%s)%s;", externName, externCallArgs, raise)
		}
		g.indent--
	}
	g.line("}")
	g.blank()

	// Private extern
	g.line("%s(%s)%s", externName, externParams, externRetSig)
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
	if !g.canonicalABI {
		var parts []string
		for _, p := range params {
			parts = append(parts, fmt.Sprintf("%s %s", promiseExternType(p.Type), p.Name))
		}
		return strings.Join(parts, ", ")
	}
	flat, usePtr := flattenParams(params)
	if usePtr {
		return "i32 args_ptr"
	}
	var parts []string
	for _, fp := range flat {
		parts = append(parts, fmt.Sprintf("%s %s", flatPromiseType(fp.Type), fp.Name))
	}
	return strings.Join(parts, ", ")
}

func (g *generator) formatExternCallArgs(params []Param) string {
	var parts []string
	for _, p := range params {
		if !g.canonicalABI && isRefType(p.Type) {
			parts = append(parts, p.Name+"._handle")
		} else {
			parts = append(parts, p.Name)
		}
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

// formatExternReturnType converts results to extern return type, mapping resource types to i32.
func (g *generator) formatExternReturnType(results []TypeRef) string {
	if len(results) == 0 {
		return ""
	}
	if len(results) == 1 {
		ref := results[0]
		if ref.Kind == ResultKind {
			if ref.Ok != nil {
				return promiseExternType(*ref.Ok)
			}
			return ""
		}
		return promiseExternType(ref)
	}
	var parts []string
	for _, r := range results {
		parts = append(parts, promiseExternType(r))
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

// promiseExternType converts a TypeRef to a Promise type for extern declarations.
// Resource types (NamedKind) become i32 (handle), optional resource types become i32
// (0 = null). All other types use the normal promiseType conversion.
// Handles are i32 on WASM (32-bit ref table indices), not int (which is i64).
func promiseExternType(ref TypeRef) string {
	switch ref.Kind {
	case NamedKind:
		return "i32"
	case OptionKind:
		if ref.Elem != nil && ref.Elem.Kind == NamedKind {
			return "i32"
		}
		return promiseType(ref)
	default:
		return promiseType(ref)
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

// resourceReturnType returns the resource type name if the function returns a single
// NamedKind type (resource). Used to detect when wrapper must construct from handle.
func resourceReturnType(results []TypeRef) (string, bool) {
	if len(results) != 1 {
		return "", false
	}
	ref := results[0]
	if ref.Kind == ResultKind && ref.Ok != nil {
		ref = *ref.Ok
	}
	if ref.Kind == NamedKind {
		return ref.Name, true
	}
	return "", false
}

// optionResourceReturnType returns the resource type name if the function returns
// an optional resource (OptionKind wrapping NamedKind). The extern returns i32
// where 0 = null, and the wrapper checks handle before constructing.
func optionResourceReturnType(results []TypeRef) (string, bool) {
	if len(results) != 1 {
		return "", false
	}
	ref := results[0]
	if ref.Kind == ResultKind && ref.Ok != nil {
		ref = *ref.Ok
	}
	if ref.Kind == OptionKind && ref.Elem != nil && ref.Elem.Kind == NamedKind {
		return ref.Elem.Name, true
	}
	return "", false
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

// emitCanonicalABIHelpers emits private extern declarations for canonical ABI
// helper functions provided by the WASM runtime (wasm_alloc.o).
func (g *generator) emitCanonicalABIHelpers(listElemTypes []TypeRef) {
	g.line("// --- Canonical ABI helper functions (provided by runtime) ---")
	g.blank()
	g.line("_cabi_load_i32(i32 ptr) i32 `extern(\"cabi_load_i32\") `target(%s);", g.target)
	g.line("_cabi_load_i64(i32 ptr) i64 `extern(\"cabi_load_i64\") `target(%s);", g.target)
	g.line("_cabi_load_f32(i32 ptr) f32 `extern(\"cabi_load_f32\") `target(%s);", g.target)
	g.line("_cabi_load_f64(i32 ptr) f64 `extern(\"cabi_load_f64\") `target(%s);", g.target)
	g.line("_cabi_store_i32(i32 ptr, i32 val) `extern(\"cabi_store_i32\") `target(%s);", g.target)
	g.line("_cabi_store_i64(i32 ptr, i64 val) `extern(\"cabi_store_i64\") `target(%s);", g.target)
	g.line("_cabi_store_f32(i32 ptr, f32 val) `extern(\"cabi_store_f32\") `target(%s);", g.target)
	g.line("_cabi_store_f64(i32 ptr, f64 val) `extern(\"cabi_store_f64\") `target(%s);", g.target)
	g.line("_cabi_string_data(string s) i32 `extern(\"cabi_string_data\") `target(%s);", g.target)
	g.line("_cabi_string_len(string s) i32 `extern(\"cabi_string_len\") `target(%s);", g.target)
	g.line("_cabi_string_from(i32 ptr, i32 len) string `extern(\"cabi_string_from\") `target(%s);", g.target)
	g.line("_cabi_retarea_ptr() i32 `extern(\"cabi_retarea_ptr\") `target(%s);", g.target)
	// Per-element-type vector helpers
	for _, elem := range listElemTypes {
		suffix := vectorHelperSuffix(elem)
		vecType := promiseType(elem) + "[]"
		g.line("_cabi_vector_data_%s(%s v) i32 `extern(\"cabi_vector_data\") `target(%s);", suffix, vecType, g.target)
		g.line("_cabi_vector_len_%s(%s v) i32 `extern(\"cabi_vector_len\") `target(%s);", suffix, vecType, g.target)
		g.line("_cabi_vector_from_%s(i32 ptr, i32 len, i32 elem_size) %s `extern(\"cabi_vector_from\") `target(%s);", suffix, vecType, g.target)
	}
	g.blank()
}

// formatExternReturnSig generates the return signature for a canonical ABI extern.
// If the return requires a retptr (> MaxFlatResults), the extern returns void
// and the retptr is added as the last param.
func (g *generator) formatExternReturnSig(results []TypeRef) string {
	if !g.canonicalABI {
		retType := g.formatExternReturnType(results)
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
	_, useRetPtr := flattenResults(results)
	if useRetPtr {
		// retptr pattern: extern returns void (retptr added as last param)
		if isFailable(results) {
			return "" // void, but wrapper is failable
		}
		return ""
	}
	// Direct return: use the flat type
	flat, _ := flattenResults(results)
	if len(flat) == 0 {
		if isFailable(results) {
			return "!"
		}
		return ""
	}
	if len(flat) == 1 {
		retType := flatPromiseType(flat[0])
		if isFailable(results) {
			return " " + retType + "!"
		}
		return " " + retType
	}
	// Multiple flat results (shouldn't reach here since useRetPtr would be true)
	return g.formatReturnSig(results)
}

// formatCanonicalCallArgs generates the arguments for calling a canonical ABI extern
// from a wrapper function, including type lowering for compound types.
func (g *generator) formatCanonicalCallArgs(params []Param, hasRetPtr bool) string {
	if !g.canonicalABI {
		return g.formatExternCallArgs(params)
	}
	var parts []string
	for _, p := range params {
		if needsCanonicalLowering(p.Type) {
			// Compound type: lower to flat values
			parts = append(parts, g.lowerParamToFlat(p)...)
		} else {
			parts = append(parts, p.Name)
		}
	}
	if hasRetPtr {
		parts = append(parts, "_cabi_retarea_ptr()")
	}
	return strings.Join(parts, ", ")
}

// lowerParamToFlat generates the expression(s) to lower a Promise param to canonical ABI flat values.
func (g *generator) lowerParamToFlat(p Param) []string {
	switch {
	case p.Type.Kind == BuiltinKind && p.Type.Builtin == "string":
		return []string{
			fmt.Sprintf("_cabi_string_data(%s)", p.Name),
			fmt.Sprintf("_cabi_string_len(%s)", p.Name),
		}
	case p.Type.Kind == ListKind:
		suffix := vectorHelperSuffix(*p.Type.Elem)
		return []string{
			fmt.Sprintf("_cabi_vector_data_%s(%s)", suffix, p.Name),
			fmt.Sprintf("_cabi_vector_len_%s(%s)", suffix, p.Name),
		}
	default:
		// For types we can't lower yet, pass as-is (will cause compile error)
		return []string{p.Name}
	}
}

// formatExternParamsWithRetPtr generates extern params including retptr when needed.
func (g *generator) formatExternParamsWithRetPtr(params []Param, results []TypeRef) string {
	if !g.canonicalABI {
		return g.formatExternParams(params)
	}
	base := g.formatExternParams(params)
	_, useRetPtr := flattenResults(results)
	if useRetPtr {
		if base == "" {
			return "i32 retptr"
		}
		return base + ", i32 retptr"
	}
	return base
}

// liftReturnFromRetPtr generates wrapper code to read and lift the return value
// from the canonical ABI retptr area.
func (g *generator) liftReturnFromRetPtr(results []TypeRef) string {
	if len(results) == 0 {
		return ""
	}
	ref := results[0]
	if ref.Kind == ResultKind {
		// result<T, E>: payload offset is union-aligned across Ok and Err
		result := ref
		ref = *ref.Ok
		offset := resultPayloadOffset(result)
		if ref.Kind == BuiltinKind && ref.Builtin == "string" {
			return fmt.Sprintf("_cabi_string_from(_cabi_load_i32(_cabi_retarea_ptr() + %d), _cabi_load_i32(_cabi_retarea_ptr() + %d))",
				offset, offset+4)
		}
		if ref.Kind == ListKind {
			suffix := vectorHelperSuffix(*ref.Elem)
			elemSize := witElemSize(ref.Elem.Builtin)
			return fmt.Sprintf("_cabi_vector_from_%s(_cabi_load_i32(_cabi_retarea_ptr() + %d), _cabi_load_i32(_cabi_retarea_ptr() + %d), %d)",
				suffix, offset, offset+4, elemSize)
		}
		return liftScalarFromRetPtr(ref, offset)
	}
	if ref.Kind == BuiltinKind && ref.Builtin == "string" {
		return "_cabi_string_from(_cabi_load_i32(_cabi_retarea_ptr()), _cabi_load_i32(_cabi_retarea_ptr() + 4))"
	}
	if ref.Kind == ListKind {
		suffix := vectorHelperSuffix(*ref.Elem)
		elemSize := witElemSize(ref.Elem.Builtin)
		return fmt.Sprintf("_cabi_vector_from_%s(_cabi_load_i32(_cabi_retarea_ptr()), _cabi_load_i32(_cabi_retarea_ptr() + 4), %d)",
			suffix, elemSize)
	}
	return liftScalarFromRetPtr(ref, 0)
}

// liftScalarFromRetPtr generates a load expression for a scalar at a retptr offset.
func liftScalarFromRetPtr(ref TypeRef, offset int) string {
	if ref.Kind != BuiltinKind {
		return fmt.Sprintf("_cabi_load_i32(_cabi_retarea_ptr() + %d)", offset)
	}
	offsetExpr := fmt.Sprintf("_cabi_retarea_ptr() + %d", offset)
	if offset == 0 {
		offsetExpr = "_cabi_retarea_ptr()"
	}
	switch ref.Builtin {
	case "u64", "s64":
		return fmt.Sprintf("_cabi_load_i64(%s)", offsetExpr)
	case "f32":
		return fmt.Sprintf("_cabi_load_f32(%s)", offsetExpr)
	case "f64":
		return fmt.Sprintf("_cabi_load_f64(%s)", offsetExpr)
	default:
		return fmt.Sprintf("_cabi_load_i32(%s)", offsetExpr)
	}
}

// liftErrFromRetPtr generates an expression for the error message extracted from
// the canonical ABI retarea when a result<T, E> returns an error (discriminant != 0).
func (g *generator) liftErrFromRetPtr(result TypeRef) string {
	if result.Err == nil {
		return `"component error"`
	}
	errRef := *result.Err
	offset := resultPayloadOffset(result)
	if errRef.Kind == BuiltinKind && errRef.Builtin == "string" {
		// String error: lift string directly as error message
		return fmt.Sprintf("_cabi_string_from(_cabi_load_i32(_cabi_retarea_ptr() + %d), _cabi_load_i32(_cabi_retarea_ptr() + %d))",
			offset, offset+4)
	}
	// Scalar/named error: lift value and stringify
	loadExpr := liftScalarFromRetPtr(errRef, offset)
	return fmt.Sprintf(`"component error: " + %s.to_string()`, loadExpr)
}

// collectListElemTypes finds all unique list element types used across all modules.
// Recursively scans function params and results (including inside result<>, option<>, tuple).
func collectListElemTypes(modules []*Module) []TypeRef {
	seen := make(map[string]bool)
	var result []TypeRef

	var visit func(ref TypeRef)
	visit = func(ref TypeRef) {
		switch ref.Kind {
		case ListKind:
			if ref.Elem != nil {
				key := promiseType(*ref.Elem)
				if !seen[key] {
					seen[key] = true
					result = append(result, *ref.Elem)
				}
				visit(*ref.Elem) // nested lists
			}
		case OptionKind:
			if ref.Elem != nil {
				visit(*ref.Elem)
			}
		case ResultKind:
			if ref.Ok != nil {
				visit(*ref.Ok)
			}
			if ref.Err != nil {
				visit(*ref.Err)
			}
		case TupleKind:
			for _, e := range ref.Elements {
				visit(e)
			}
		}
	}

	for _, m := range modules {
		for _, f := range m.Functions {
			for _, p := range f.Params {
				visit(p.Type)
			}
			for _, r := range f.Results {
				visit(r)
			}
		}
		for _, r := range m.Resources {
			for _, method := range r.Methods {
				for _, p := range method.Params {
					visit(p.Type)
				}
				for _, res := range method.Results {
					visit(res)
				}
			}
		}
	}

	return result
}

// resultPayloadOffset returns the canonical ABI payload offset for a result<T, E>.
// The payload is a union of Ok and Err — both start at the same offset, determined
// by max(align(T), align(E)). 64-bit types require alignment 8; everything else 4.
func resultPayloadOffset(result TypeRef) int {
	align := 4
	for _, ref := range []*TypeRef{result.Ok, result.Err} {
		if ref != nil && ref.Kind == BuiltinKind {
			switch ref.Builtin {
			case "u64", "s64", "f64":
				if align < 8 {
					align = 8
				}
			}
		}
	}
	return align
}
