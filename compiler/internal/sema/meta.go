package sema

import (
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
	"native":       {TargetMethod},
	"copy":         {TargetType, TargetEnum},
	"structural":   {TargetType},
	"doc":          {TargetType, TargetField, TargetMethod, TargetFunc, TargetEnum},
	"deprecated":   {TargetType, TargetField, TargetMethod, TargetFunc, TargetEnum},
	"test":         {TargetFunc},
	"inline":       {TargetFunc, TargetMethod},
	"packed":       {TargetType},
	"align":        {TargetType},
	"extern":       {TargetFunc},
	"serializable": {TargetType, TargetEnum},
	"public":       {TargetType, TargetField, TargetMethod, TargetFunc, TargetEnum},
	"unsafe":       {TargetFunc, TargetMethod},
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
			return stringLitValue(ann.Params[0].Value)
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
	}
	return false
}
