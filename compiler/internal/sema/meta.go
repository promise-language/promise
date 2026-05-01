package sema

import (
	"strings"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/types"
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
	default:
		return "declaration"
	}
}

// builtinMetas maps known meta names to their allowed targets.
var builtinMetas = map[string][]MetaTarget{
	"value":        {TargetField, TargetMethod},
	"instance":     {TargetField, TargetMethod},
	"variant":      {TargetField, TargetMethod},
	"type":         {TargetField, TargetMethod},
	"raw":          {TargetField},
	"abstract":     {TargetMethod},
	"native":       {TargetMethod, TargetType},
	"copy":         {TargetType, TargetEnum},
	"structural":   {TargetType},
	"doc":          {TargetType, TargetField, TargetMethod, TargetFunc, TargetEnum, TargetParam, TargetVariant},
	"deprecated":   {TargetType, TargetField, TargetMethod, TargetFunc, TargetEnum, TargetParam, TargetVariant},
	"test":         {TargetFunc},
	"inline":       {TargetFunc, TargetMethod},
	"packed":       {TargetType},
	"align":        {TargetType},
	"extern":       {TargetFunc},
	"serializable": {TargetType, TargetEnum},
	"public":       {TargetType, TargetField, TargetMethod, TargetFunc, TargetEnum},
	"unsafe":       {TargetFunc, TargetMethod},
	"final":        {TargetField},
	"factory":      {TargetMethod},
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

// extractTestExclude extracts the exclude targets from a `test(exclude: "wasm32") annotation.
// The value is split by comma into a list of target substrings.
func extractTestExclude(annotations []*ast.MetaAnnotation) []string {
	for _, ann := range annotations {
		if ann.Name != "test" {
			continue
		}
		for _, p := range ann.Params {
			if p.Name == "exclude" {
				raw := evalStringLit(p.Value)
				if raw == "" {
					return nil
				}
				var targets []string
				for _, t := range strings.Split(raw, ",") {
					t = strings.TrimSpace(t)
					if t != "" {
						targets = append(targets, t)
					}
				}
				return targets
			}
		}
	}
	return nil
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
		td, ok := decl.(*ast.TypeDecl)
		if !ok {
			continue
		}
		obj := c.fileScope.Lookup(td.Name)
		if obj == nil {
			// Try stdScope for std types
			obj = c.stdScope.Lookup(td.Name)
		}
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
		for _, p := range named.Parents() {
			if p.HasNew() && !named.HasNew() {
				c.errorf(td.Pos(), "type %s must define new() because parent %s defines new()", td.Name, p)
				break
			}
			if p.HasNew() && named.HasNew() {
				parentNew := lookupOwnMethod(p, "new")
				childNew := lookupOwnMethod(named, "new")
				if parentNew != nil && parentNew.Sig().CanError() &&
					(childNew == nil || !childNew.Sig().CanError()) {
					c.errorf(td.Pos(), "new() on %s must be failable because parent %s has failable new()", td.Name, p)
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
