package sema

import (
	"strings"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// MetaTarget represents the kind of declaration a meta annotation is attached to.
type MetaTarget int

const (
	TargetType MetaTarget = iota
	TargetField
	TargetMethod
	TargetFunc
	TargetEnum
	TargetParam
	TargetVariant
	TargetReturn
)

func targetLabel(t MetaTarget) string {
	switch t {
	case TargetType:
		return "type"
	case TargetField:
		return "field"
	case TargetMethod:
		return "method"
	case TargetFunc:
		return "function"
	case TargetEnum:
		return "enum"
	case TargetParam:
		return "parameter"
	case TargetVariant:
		return "variant"
	case TargetReturn:
		return "return type"
	default:
		return "declaration"
	}
}

// builtinMetas maps known meta names to their allowed targets.
var builtinMetas = map[string][]MetaTarget{
	"value":        {TargetField, TargetMethod},
	"instance":     {TargetField, TargetMethod},
	"variant":      {TargetField},
	"global":       {TargetMethod},
	"mono":         {TargetMethod},
	"raw":          {TargetField},
	"abstract":     {TargetMethod},
	"native":       {TargetMethod, TargetType},
	"copy":         {TargetType, TargetEnum},
	"clone":        {TargetType, TargetEnum},
	"structural":   {TargetType},
	"doc":          {TargetType, TargetField, TargetMethod, TargetFunc, TargetEnum, TargetParam, TargetVariant},
	"deprecated":   {TargetType, TargetField, TargetMethod, TargetFunc, TargetEnum, TargetParam, TargetVariant},
	"test":         {TargetFunc},
	"inline":       {TargetFunc, TargetMethod},
	"packed":       {TargetType},
	"align":        {TargetType},
	"extern":       {TargetFunc},
	"target":       {TargetType, TargetEnum, TargetFunc},
	"serializable": {TargetType, TargetEnum},
	"key":          {TargetField, TargetVariant},
	"skip":         {TargetField},
	"include_none": {TargetField},
	"required":     {TargetField},
	"flatten":      {TargetField},
	"public":       {TargetType, TargetField, TargetMethod, TargetFunc, TargetEnum},
	"unsafe":       {TargetFunc, TargetMethod},
	"final":        {TargetField},
	"factory":      {TargetMethod},
	"embed":        {TargetFunc},
	"wasm_import":  {TargetFunc},
	"lifetime":     {TargetParam, TargetFunc, TargetMethod},
	"sendable":     {TargetType, TargetEnum},
	"sharable":     {TargetType, TargetEnum},
	"not_sendable": {TargetType, TargetEnum},
	"not_sharable": {TargetType, TargetEnum},
}

// validateMetas checks that all meta annotations on a declaration are valid:
// known names, correct targets, no duplicates.
func (c *Checker) validateMetas(annotations []*ast.MetaAnnotation, target MetaTarget) {
	seen := make(map[string]bool)
	for _, ann := range annotations {
		if seen[ann.Name] {
			c.errorf(ann.Pos(), "duplicate meta annotation `%s", ann.Name)
			continue
		}
		seen[ann.Name] = true

		allowed, known := builtinMetas[ann.Name]
		if !known {
			c.errorf(ann.Pos(), "unknown meta annotation `%s", ann.Name)
			continue
		}
		if !targetAllowed(allowed, target) {
			c.errorf(ann.Pos(), "meta `%s cannot be applied to %s", ann.Name, targetLabel(target))
		}

		// Check for duplicate named parameters within this annotation.
		seenParams := make(map[string]bool)
		for _, p := range ann.Params {
			if p.Name == "" {
				continue // positional params don't have names to conflict
			}
			if seenParams[p.Name] {
				c.errorf(p.Pos(), "duplicate annotation parameter '%s' in `%s", p.Name, ann.Name)
				continue
			}
			seenParams[p.Name] = true
		}
	}
}

func targetAllowed(allowed []MetaTarget, target MetaTarget) bool {
	for _, a := range allowed {
		if a == target {
			return true
		}
	}
	return false
}

// extractDoc returns the doc string from a `doc annotation, or "".
func extractDoc(annotations []*ast.MetaAnnotation) string {
	for _, ann := range annotations {
		if ann.Name != "doc" {
			continue
		}
		if len(ann.Params) > 0 {
			return evalStringLit(ann.Params[0].Value)
		}
		return ""
	}
	return ""
}

// extractLifetime returns the lifetime name from a `lifetime(name) annotation, or "".
// The parameter must be a single identifier (not a string literal).
func extractLifetime(annotations []*ast.MetaAnnotation) string {
	for _, ann := range annotations {
		if ann.Name != "lifetime" {
			continue
		}
		if len(ann.Params) == 1 {
			if ident, ok := ann.Params[0].Value.(*ast.IdentExpr); ok {
				return ident.Name
			}
		}
		return ""
	}
	return ""
}

// isRefParam returns true if a parameter is a reference parameter,
// either via explicit Ref modifier (&/~) or via reference type (SharedRef/MutRef).
func isRefParam(p *types.Param) bool {
	if p.Ref() == types.RefShared || p.Ref() == types.RefMut {
		return true
	}
	switch p.Type().(type) {
	case *types.SharedRef, *types.MutRef:
		return true
	}
	return false
}

// isRefResultType returns true if a type is a reference type.
func isRefResultType(t types.Type) bool {
	if t == nil {
		return false
	}
	switch t.(type) {
	case *types.SharedRef, *types.MutRef:
		return true
	}
	return false
}

// validateLifetimes validates `lifetime annotations on parameters and the function/method.
// It checks:
// - `lifetime on a param requires the param to be a reference type
// - `lifetime on a function/method requires a reference return type
// - The return lifetime name must match a declared parameter lifetime
// - `lifetime must have exactly one identifier parameter
func (c *Checker) validateLifetimes(sig *types.Signature, funcAnnotations []*ast.MetaAnnotation, astParams []*ast.Param) {
	// Validate `lifetime annotation format on parameters
	for i, p := range astParams {
		for _, ann := range p.Annotations {
			if ann.Name != "lifetime" {
				continue
			}
			// Check format: must have exactly one positional identifier parameter
			if len(ann.Params) != 1 || ann.Params[0].Name != "" {
				c.errorf(ann.Pos(), "`lifetime requires exactly one identifier parameter, e.g. `lifetime(a)")
				continue
			}
			if _, ok := ann.Params[0].Value.(*ast.IdentExpr); !ok {
				c.errorf(ann.Pos(), "`lifetime parameter must be an identifier, e.g. `lifetime(a)")
				continue
			}
			// Check that the param is a reference type
			if i < len(sig.Params()) && !isRefParam(sig.Params()[i]) {
				c.errorf(ann.Pos(), "`lifetime can only be applied to reference parameters")
			}
		}
	}

	// Validate `lifetime on the function/method (refers to return type)
	for _, ann := range funcAnnotations {
		if ann.Name != "lifetime" {
			continue
		}
		// Check format
		if len(ann.Params) != 1 || ann.Params[0].Name != "" {
			c.errorf(ann.Pos(), "`lifetime requires exactly one identifier parameter, e.g. `lifetime(a)")
			continue
		}
		if _, ok := ann.Params[0].Value.(*ast.IdentExpr); !ok {
			c.errorf(ann.Pos(), "`lifetime parameter must be an identifier, e.g. `lifetime(a)")
			continue
		}

		// Check that return type is a reference
		if !isRefResultType(sig.Result()) {
			c.errorf(ann.Pos(), "`lifetime on function refers to return type but return type is not a reference")
			continue
		}

		// Check that the lifetime name matches a parameter lifetime
		lt := sig.ResultLifetime()
		if lt == "" {
			continue
		}
		found := false
		for _, p := range sig.Params() {
			if p.Lifetime() == lt {
				found = true
				break
			}
		}
		if !found {
			c.errorf(ann.Pos(), "unknown lifetime '%s'; no parameter declares this lifetime", lt)
		}
	}
}

// extractDeprecated returns the deprecation message from a `deprecated annotation.
// Returns "" if not deprecated. Returns " " if deprecated with no message.
func extractDeprecated(annotations []*ast.MetaAnnotation) string {
	for _, ann := range annotations {
		if ann.Name != "deprecated" {
			continue
		}
		if len(ann.Params) > 0 {
			msg := stringLitValue(ann.Params[0].Value)
			if msg != "" {
				return msg
			}
		}
		return " " // deprecated with no message
	}
	return ""
}

// stringLitValue extracts a plain string value from a StringLit expression.
// Only text parts are concatenated; interpolation parts are silently dropped.
func stringLitValue(expr ast.Expr) string {
	sl, ok := expr.(*ast.StringLit)
	if !ok {
		return ""
	}
	var s string
	for _, p := range sl.Parts {
		if t, ok := p.(ast.StringText); ok {
			s += t.Text
		}
	}
	return s
}

// evalStringLit extracts a string value from a StringLit expression, resolving escape sequences.
// For triple-quoted and raw strings, extracts content from the Raw field (Parts is empty).
func evalStringLit(expr ast.Expr) string {
	sl, ok := expr.(*ast.StringLit)
	if !ok {
		return ""
	}
	switch sl.Kind {
	case ast.StringTriple:
		if len(sl.Raw) >= 6 {
			return sl.Raw[3 : len(sl.Raw)-3]
		}
		return ""
	case ast.StringRaw:
		if len(sl.Raw) >= 3 {
			return sl.Raw[2 : len(sl.Raw)-1]
		}
		return ""
	}
	var buf strings.Builder
	for _, p := range sl.Parts {
		switch p := p.(type) {
		case ast.StringText:
			buf.WriteString(p.Text)
		case ast.StringEscape:
			buf.WriteString(resolveEscape(p.Sequence))
		}
	}
	return buf.String()
}

// resolveEscape converts an escape sequence token to its string value.
func resolveEscape(seq string) string {
	if len(seq) > 1 && seq[0] == '\\' {
		seq = seq[1:]
	}
	switch seq {
	case "n":
		return "\n"
	case "t":
		return "\t"
	case "r":
		return "\r"
	case "b":
		return "\b"
	case "\\":
		return "\\"
	case "\"":
		return "\""
	case "0":
		return "\x00"
	case "{":
		return "{"
	default:
		return "\\" + seq
	}
}

// extractKey returns the key name from a `key("name") annotation, or "".
func extractKey(annotations []*ast.MetaAnnotation) string {
	for _, ann := range annotations {
		if ann.Name != "key" {
			continue
		}
		if len(ann.Params) > 0 {
			return evalStringLit(ann.Params[0].Value)
		}
		return ""
	}
	return ""
}

// extractSerializableTag extracts the tag parameter from a `serializable(tag: "kind") annotation.
// Returns the custom discriminator key name, or "" if not specified (default "type").
func extractSerializableTag(annotations []*ast.MetaAnnotation) string {
	for _, ann := range annotations {
		if ann.Name != "serializable" {
			continue
		}
		for _, p := range ann.Params {
			if p.Name == "tag" {
				return evalStringLit(p.Value)
			}
		}
	}
	return ""
}

// extractTestExpected extracts the expected output from a `test(expected="...") annotation.
// Returns the evaluated string and true if the annotation has an expected parameter.
func extractTestExpected(annotations []*ast.MetaAnnotation) (string, bool) {
	for _, ann := range annotations {
		if ann.Name != "test" {
			continue
		}
		for _, p := range ann.Params {
			if p.Name == "expected" {
				return evalStringLit(p.Value), true
			}
		}
	}
	return "", false
}

// extractTestTimeout extracts the timeout duration from a `test(timeout: "5s") annotation.
// Returns the raw duration string and true if the annotation has a timeout parameter.
func extractTestTimeout(annotations []*ast.MetaAnnotation) (string, bool) {
	for _, ann := range annotations {
		if ann.Name != "test" {
			continue
		}
		for _, p := range ann.Params {
			if p.Name == "timeout" {
				return evalStringLit(p.Value), true
			}
		}
	}
	return "", false
}

// extractTestMemoryLimit extracts the memory-limit size string from a
// `test(memory_limit: "256MB") annotation (T0689). Returns the raw string
// and true if the annotation has a memory_limit parameter.
func extractTestMemoryLimit(annotations []*ast.MetaAnnotation) (string, bool) {
	for _, ann := range annotations {
		if ann.Name != "test" {
			continue
		}
		for _, p := range ann.Params {
			if p.Name == "memory_limit" {
				return evalStringLit(p.Value), true
			}
		}
	}
	return "", false
}

// isValidMemoryLimitLiteral returns true if s is a syntactically well-formed
// memory limit literal: "0" (opt-out) or a non-negative integer followed by a
// unit suffix (B/KB/MB/GB/KiB/MiB/GiB, case-insensitive). Used by sema for
// early validation of `test(memory_limit: "...") annotations (T0689). The
// authoritative parser is parseMemoryLimitArg in cmd/promise/main.go.
func isValidMemoryLimitLiteral(s string) bool {
	if s == "" {
		return false
	}
	if s == "0" {
		return true
	}
	lower := strings.ToLower(s)
	suffixes := []string{"gib", "mib", "kib", "gb", "mb", "kb", "b"}
	var numPart string
	matched := false
	for _, suf := range suffixes {
		if strings.HasSuffix(lower, suf) {
			numPart = strings.TrimSpace(strings.TrimSuffix(lower, suf))
			matched = true
			break
		}
	}
	if !matched || numPart == "" {
		return false
	}
	for _, ch := range numPart {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// extractTestExclude extracts the exclude target identifiers from a `test(exclude: wasm) annotation.
// The value may be a single identifier or an || expression of identifiers.
func extractTestExclude(annotations []*ast.MetaAnnotation) []string {
	for _, ann := range annotations {
		if ann.Name != "test" {
			continue
		}
		for _, p := range ann.Params {
			if p.Name == "exclude" {
				return collectExcludeIdents(p.Value)
			}
		}
	}
	return nil
}

// collectExcludeIdents recursively gathers identifier names from || expressions.
func collectExcludeIdents(expr ast.Expr) []string {
	switch e := expr.(type) {
	case *ast.IdentExpr:
		return []string{e.Name}
	case *ast.BinaryExpr:
		if e.Op == ast.BinOr {
			return append(collectExcludeIdents(e.Left), collectExcludeIdents(e.Right)...)
		}
	}
	return nil
}

// validateTestExclude validates the exclude parameter(s) of a `test annotation.
func (c *Checker) validateTestExclude(annotations []*ast.MetaAnnotation) {
	for _, ann := range annotations {
		if ann.Name != "test" {
			continue
		}
		for _, p := range ann.Params {
			if p.Name == "exclude" {
				c.validateExcludeExpr(p.Value)
			}
		}
	}
}

// validateExcludeExpr validates an exclude expression (identifier or || of identifiers).
func (c *Checker) validateExcludeExpr(expr ast.Expr) {
	switch e := expr.(type) {
	case *ast.IdentExpr:
		if !ValidExcludeIdents[e.Name] {
			c.errorf(e.Pos(), "unknown exclude target %q; valid identifiers: windows, linux, macos, wasm, wasi, web, posix, x86_64, aarch64, arm64", e.Name)
		}
	case *ast.BinaryExpr:
		if e.Op == ast.BinOr {
			c.validateExcludeExpr(e.Left)
			c.validateExcludeExpr(e.Right)
		} else {
			c.errorf(expr.Pos(), "exclude expression must use || to combine target identifiers")
		}
	case *ast.StringLit:
		c.errorf(expr.Pos(), "exclude target must be an identifier, not a string literal (e.g., exclude: wasm instead of exclude: \"wasm32\")")
	default:
		c.errorf(expr.Pos(), "invalid exclude expression; expected identifier or identifier || identifier")
	}
}

// extractTestAllowLeaks extracts the allow_leaks flag from a `test(allow_leaks: true) annotation.
// Returns true if the annotation has allow_leaks set to true.
func extractTestAllowLeaks(annotations []*ast.MetaAnnotation) bool {
	for _, ann := range annotations {
		if ann.Name != "test" {
			continue
		}
		for _, p := range ann.Params {
			if p.Name == "allow_leaks" {
				if bl, ok := p.Value.(*ast.BoolLit); ok && bl.Value {
					return true
				}
			}
		}
	}
	return false
}

// extractEmbedPath extracts the file path from a `embed("path") annotation.
// Returns the path string and true if the annotation is present.
func extractEmbedPath(annotations []*ast.MetaAnnotation) (string, bool) {
	for _, ann := range annotations {
		if ann.Name != "embed" {
			continue
		}
		if len(ann.Params) > 0 {
			// First positional parameter is the file path
			for _, p := range ann.Params {
				if p.Name == "" {
					return evalStringLit(p.Value), true
				}
			}
		}
		return "", true // `embed with no path — will be caught as an error
	}
	return "", false
}

// extractEmbedCompress returns true if the `embed annotation has compress: true.
func extractEmbedCompress(annotations []*ast.MetaAnnotation) bool {
	for _, ann := range annotations {
		if ann.Name != "embed" {
			continue
		}
		for _, p := range ann.Params {
			if p.Name == "compress" {
				// Check for boolean true literal
				if bl, ok := p.Value.(*ast.BoolLit); ok && bl.Value {
					return true
				}
			}
		}
	}
	return false
}

// validateWasmImport checks that `wasm_import is used correctly on a function declaration.
// Rules: must be on an extern function (no body), must have exactly 2 string parameters
// (module name and import name). Warns if used without `target(wasm).
func (c *Checker) validateWasmImport(d *ast.FuncDecl) {
	var ann *ast.MetaAnnotation
	for _, a := range d.Annotations {
		if a.Name == "wasm_import" {
			ann = a
			break
		}
	}
	if ann == nil {
		return
	}

	// Must be on an extern function (no body)
	if d.Body != nil {
		c.errorf(ann.Pos(), "`wasm_import can only be applied to extern functions")
		return
	}

	// Must also have `extern
	if !c.hasAnnotation(d.Annotations, "extern") {
		c.errorf(ann.Pos(), "`wasm_import requires `extern annotation")
		return
	}

	// Must have exactly 2 positional string parameters
	if len(ann.Params) != 2 {
		c.errorf(ann.Pos(), "`wasm_import requires exactly 2 parameters: module name and import name")
		return
	}
	for i, p := range ann.Params {
		if _, ok := p.Value.(*ast.StringLit); !ok {
			label := "module name"
			if i == 1 {
				label = "import name"
			}
			c.errorf(p.Pos(), "`wasm_import %s must be a string literal", label)
		}
	}

	// Warn if no `target(wasm) — annotation will be ignored on non-WASM targets
	hasWasmTarget := false
	for _, a := range d.Annotations {
		if a.Name == "target" && len(a.Params) > 0 {
			hasWasmTarget = c.exprMentionsWasm(a.Params[0].Value)
			break
		}
	}
	if !hasWasmTarget {
		c.warnf(ann.Pos(), "`wasm_import without `target(wasm) will be ignored on non-WASM targets")
	}
}

// exprMentionsWasm returns true if a target condition expression references "wasm", "wasi", or "web".
func (c *Checker) exprMentionsWasm(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.IdentExpr:
		return e.Name == "wasm" || e.Name == "wasi" || e.Name == "web"
	case *ast.UnaryExpr:
		return c.exprMentionsWasm(e.Operand)
	case *ast.BinaryExpr:
		return c.exprMentionsWasm(e.Left) || c.exprMentionsWasm(e.Right)
	}
	return false
}

// ExtractWasmImport extracts the module and import name from a `wasm_import annotation.
// Returns ("", "") if the annotation is not present.
func ExtractWasmImport(annotations []*ast.MetaAnnotation) (string, string) {
	for _, ann := range annotations {
		if ann.Name != "wasm_import" {
			continue
		}
		if len(ann.Params) >= 2 {
			mod := evalStringLit(ann.Params[0].Value)
			name := evalStringLit(ann.Params[1].Value)
			return mod, name
		}
	}
	return "", ""
}

// checkDeprecatedObj emits a warning if the resolved object refers to a deprecated entity.
func (c *Checker) checkDeprecatedObj(pos ast.Pos, obj types.Object) {
	switch o := obj.(type) {
	case *types.TypeName:
		switch t := o.Type().(type) {
		case *types.Named:
			if t.Deprecated() != "" {
				c.warnf(pos, "use of deprecated type '%s'", o.Name())
			}
		case *types.Enum:
			if t.Deprecated() != "" {
				c.warnf(pos, "use of deprecated enum '%s'", o.Name())
			}
		}
	case *types.Func:
		if o.Deprecated() != "" {
			c.warnf(pos, "use of deprecated function '%s'", o.Name())
		}
	}
}

// validateDropMethod checks that a drop() method has the required signature:
// drop(~this) — mutable borrow receiver, no parameters, no return, not failable.
func (c *Checker) validateDropMethod(named *types.Named, m *types.Method, d *ast.TypeDecl) {
	sig := m.Sig()
	if sig == nil {
		return
	}
	pos := d.Pos()
	if sig.Recv() == nil || sig.Recv().Ref() != types.RefMut {
		c.errorf(pos, "drop() method on %s must take ~this (mutable borrow receiver)", d.Name)
	}
	if len(sig.Params()) != 0 {
		c.errorf(pos, "drop() method on %s must have no parameters", d.Name)
	}
	if sig.Result() != nil && sig.Result() != types.TypVoid {
		c.errorf(pos, "drop() method on %s must not return a value", d.Name)
	}
	if sig.CanError() {
		c.errorf(pos, "drop() method on %s must not be failable", d.Name)
	}
	if m.IsAbstract() {
		c.errorf(pos, "drop() method on %s must not be abstract", d.Name)
	}
	if named.IsCopy() {
		c.errorf(pos, "copy type %s cannot have a drop() method", d.Name)
	}
	if named.InheritsFrom(types.TypError) {
		c.errorf(pos, "error type %s cannot have a drop() method (error values are not dropped when caught or propagated)", d.Name)
	}
}

// validateEnumDropMethod checks that a drop() method on an enum has the required signature:
// drop(~this) — mutable borrow receiver, no parameters, no return, not failable.
func (c *Checker) validateEnumDropMethod(enum *types.Enum, m *types.Method, d *ast.EnumDecl) {
	sig := m.Sig()
	if sig == nil {
		return
	}
	pos := d.Pos()
	if sig.Recv() == nil || sig.Recv().Ref() != types.RefMut {
		c.errorf(pos, "drop() method on %s must take ~this (mutable borrow receiver)", d.Name)
	}
	if len(sig.Params()) != 0 {
		c.errorf(pos, "drop() method on %s must have no parameters", d.Name)
	}
	if sig.Result() != nil && sig.Result() != types.TypVoid {
		c.errorf(pos, "drop() method on %s must not return a value", d.Name)
	}
	if sig.CanError() {
		c.errorf(pos, "drop() method on %s must not be failable", d.Name)
	}
	if enum.IsCopy() {
		c.errorf(pos, "copy type %s cannot have a drop() method", d.Name)
	}
}

// validateNewMethod checks that a new() constructor has a valid signature:
// new(params) — implicit ~this receiver, no explicit return type.
// The receiver and return type are implicit; user should not write them.
func (c *Checker) validateNewMethod(named *types.Named, m *types.Method, d *ast.TypeDecl) {
	sig := m.Sig()
	if sig == nil {
		return
	}
	pos := d.Pos()
	// new() must have a mutable receiver (implicit ~this)
	if sig.Recv() == nil || sig.Recv().Ref() != types.RefMut {
		c.errorf(pos, "new() method on %s must take ~this (mutable borrow receiver)", d.Name)
	}
	// new() must not declare an explicit return type (return is implicit Self)
	if sig.Result() != nil && sig.Result() != types.TypVoid {
		c.errorf(pos, "new() method on %s must not declare a return type (implicitly returns Self)", d.Name)
	}
	if m.IsAbstract() {
		c.errorf(pos, "new() method on %s must not be abstract", d.Name)
	}
	// Value types cannot have failable new() — codegen builds the value struct
	// inline and doesn't support error propagation in that path.
	if named.IsValueType() && sig.CanError() {
		c.errorf(pos, "value type %s cannot have a failable new() method", d.Name)
	}
}

// validateFactoryMethod checks that a `factory method has a valid signature:
// no receiver (must not declare this), return type must be Self (or implicit for abstract),
// must not be native (unless the owning type is native). Abstract is allowed only on structural interfaces.
func (c *Checker) validateFactoryMethod(named *types.Named, m *types.Method, md *ast.MethodDecl, isNativeType bool) {
	sig := m.Sig()
	if sig == nil {
		return
	}
	pos := md.Pos()
	// Factory must not have an explicit receiver
	if md.Receiver != nil {
		c.errorf(pos, "factory method %s on %s must not declare a receiver (factories have no this)", md.Name, named)
	}
	// Factory must not be abstract — unless it's on a structural interface
	if m.IsAbstract() && !named.IsStructural() {
		c.errorf(pos, "factory method %s on %s must not be abstract", md.Name, named)
	}
	if m.IsNative() && !isNativeType {
		c.errorf(pos, "factory method %s on %s must not be native", md.Name, named)
	}
	// Return type must be specified (Self or child type) — unless abstract (implicit Self)
	if !m.IsAbstract() && (sig.Result() == nil || sig.Result() == types.TypVoid) {
		c.errorf(pos, "factory method %s on %s must have a return type (typically Self)", md.Name, named)
	}
}

// validateConstructors runs after all types are defined (pass 2 complete) to check
// constructor inheritance constraints. This must be a separate pass because parent
// types may not have their HasNew() set yet during defineType if declared after children.
func (c *Checker) validateConstructors(file *ast.File) {
	for _, decl := range file.Decls {
		if c.info.FilteredDecls[decl] {
			continue
		}
		td, ok := decl.(*ast.TypeDecl)
		if !ok {
			continue
		}
		obj := c.fileScope.Lookup(td.Name)
		if obj == nil {
			continue
		}
		tn, ok := obj.(*types.TypeName)
		if !ok {
			continue
		}
		named, ok := tn.Type().(*types.Named)
		if !ok {
			continue
		}
		for _, pr := range named.Parents() {
			pn := pr.Named
			if pn.HasNew() && !named.HasNew() {
				c.errorf(td.Pos(), "type %s must define new() because parent %s defines new()", td.Name, pn)
				break
			}
			if pn.HasNew() && named.HasNew() {
				parentNew := lookupOwnMethod(pn, "new")
				childNew := lookupOwnMethod(named, "new")
				if parentNew != nil && parentNew.Sig().CanError() &&
					(childNew == nil || !childNew.Sig().CanError()) {
					c.errorf(td.Pos(), "new() on %s must be failable because parent %s has failable new()", td.Name, pn)
				}
				break
			}
		}
	}
}

// validateCopyType checks that all fields of a `copy type are themselves copy types.
func (c *Checker) validateCopyType(named *types.Named, d *ast.TypeDecl) {
	for _, f := range named.Fields() {
		if !isCopyField(f.Type()) {
			c.errorf(d.Pos(), "type %s is marked `copy but field '%s' has non-copy type %s",
				d.Name, f.Name(), f.Type())
		}
	}
}

// validateCopyEnum checks that all variant fields of a `copy enum are themselves copy types.
func (c *Checker) validateCopyEnum(enum *types.Enum, d *ast.EnumDecl) {
	for _, v := range enum.Variants() {
		for _, f := range v.Fields() {
			if !isCopyField(f.Type()) {
				c.errorf(d.Pos(), "enum %s is marked `copy but variant %s has non-copy field type %s",
					d.Name, v.Name(), f.Type())
			}
		}
	}
}

// detectValueType checks if a type is a pure value type (all fields are `value placement)
// and validates the constraints. If valid, sets IsValueType and auto-enables Copy.
func (c *Checker) detectValueType(named *types.Named, d *ast.TypeDecl) {
	// Must have at least one field, and all must be `value placed
	if named.NumFields() == 0 {
		return
	}
	for _, f := range named.Fields() {
		if f.Placement() != types.PlaceValue {
			return // not a pure value type
		}
	}

	// Native types handle their own layout — skip value type detection
	if c.hasAnnotation(d.Annotations, "native") {
		return
	}

	// Validate: value types cannot have parent types (no inheritance)
	if len(named.Parents()) > 0 {
		c.errorf(d.Pos(), "value type %s cannot have parent types (all fields are `value)", d.Name)
		return
	}

	// Validate: all `value fields must be copy types
	for _, f := range named.Fields() {
		if !isCopyField(f.Type()) {
			c.errorf(d.Pos(), "value field %s.%s must be a copy type, got %s", d.Name, f.Name(), f.Type())
			return
		}
	}

	// Validate: value types cannot have drop() methods
	if named.LookupMethod("drop") != nil {
		c.errorf(d.Pos(), "value type %s cannot have a drop() method", d.Name)
		return
	}

	// All checks passed — mark as value type and auto-enable copy
	named.SetIsValueType(true)
	if !named.IsCopy() {
		named.SetCopy(true)
	}
}

// isCopyField returns true if a type is considered copy for field validation.
// NOTE: keep in sync with ownership.isCopyType — same logic, separate package.
func isCopyField(typ types.Type) bool {
	if typ == nil {
		return false
	}
	switch typ {
	case types.TypInt, types.TypI8, types.TypI16, types.TypI32, types.TypI64,
		types.TypUint, types.TypU8, types.TypU16, types.TypU32, types.TypU64,
		types.TypF32, types.TypF64,
		types.TypBool, types.TypChar, types.TypNone, types.TypVoid:
		return true
	}
	switch t := typ.(type) {
	case *types.SharedRef, *types.MutRef:
		return true
	case *types.Named:
		return t.IsCopy()
	case *types.Enum:
		return t.IsCopy()
	case *types.Tuple:
		for _, elem := range t.Elems() {
			if !isCopyField(elem) {
				return false
			}
		}
		return true
	case *types.Optional:
		return isCopyField(t.Elem())
	case *types.Array:
		return isCopyField(t.Elem())
	case *types.TypeParam:
		// Generic type params are assumed copy — validated at instantiation
		return true
	}
	return false
}
