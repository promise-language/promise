package sema

import (
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// deferredValueType is a type whose value-type classification could not be
// decided while it was being defined, recorded in declaration order (T1527).
type deferredValueType struct {
	named *types.Named
	decl  *ast.TypeDecl
}

// deferValueType records a type for resolveInheritedValueTypes. Called only from
// detectValueType, which is the single place that decides a type's placement
// classification during the Define pass.
func (c *Checker) deferValueType(named *types.Named, d *ast.TypeDecl) {
	c.deferredValueTypes = append(c.deferredValueTypes, deferredValueType{named: named, decl: d})
}

// resolveInheritedValueTypes finishes the value-type classification that
// detectValueType had to defer (T1527): a fieldless child of a value parent is
// itself a pure value type (a layout-preserving newtype — it adds methods, not
// fields), and a type with a `value field whose type is such a newtype can only
// be validated once that newtype is classified.
//
// Runs immediately after Define — the parent chains and all field types are
// resolved by then — and before propagateDrops, which skips value types and
// would otherwise synthesize a drop for a value newtype.
//
// The marking loop is a fixpoint because a candidate may only become
// classifiable after its own parent (or value-typed field) is classified —
// A ← B ← C chains and declaration order both need iteration. It runs silently;
// diagnostics are emitted once, after convergence.
func (c *Checker) resolveInheritedValueTypes(file *ast.File) {
	for changed := true; changed; {
		changed = false
		for _, dv := range c.deferredValueTypes {
			if dv.named.IsValueType() {
				continue
			}
			if c.classifyDeferredValueType(dv.named, dv.decl, false) {
				changed = true
			}
		}
	}
	for _, dv := range c.deferredValueTypes {
		if dv.named.IsValueType() {
			continue
		}
		c.classifyDeferredValueType(dv.named, dv.decl, true)
	}
	c.checkNoStrayValueFields(file)
}

// classifyDeferredValueType decides whether a deferred type is a value type and
// marks it if so. Returns whether it was marked. Diagnostics are emitted only
// when report is true.
func (c *Checker) classifyDeferredValueType(named *types.Named, d *ast.TypeDecl, report bool) bool {
	if !hasValuePlacedField(named.AllFields()) {
		return false // ordinary heap type — deferred only because it has parents
	}

	if len(named.Parents()) == 0 {
		// Deferred only because a `value field's type was not classified yet.
		return c.markValueType(named, d, report)
	}

	// From here on the type inherits (or declares) `value fields and has parents.
	if named.NumFields() > 0 {
		if report {
			if vp := valueTypeParent(named); vp != nil {
				// The newtype form is methods-only: mixing in own fields would
				// change the layout the child shares with its value parent.
				c.errorf(ownFieldPos(named, d),
					"type %s inherits `value fields from %s and cannot declare fields of its own; a value-type child may add methods only (use a %s field instead)",
					d.Name, vp.Obj().Name(), vp.Obj().Name())
			} else {
				c.errorf(d.Pos(), "value type %s cannot have parent types (all fields are `value)", d.Name)
			}
		}
		return false
	}

	// Fieldless child of a type that contributes `value fields — the newtype
	// form, provided the parent really is a value type and neither side is
	// generic.
	parents := named.Parents()
	if len(parents) != 1 {
		if report {
			name := "a value type"
			if vp := valueTypeParent(named); vp != nil {
				name = vp.Obj().Name()
			}
			c.errorf(d.Pos(), "type %s cannot inherit from value type %s together with other parent types", d.Name, name)
		}
		return false
	}
	parent := parents[0]
	if !parent.Named.IsValueType() {
		// The parent carries `value fields but is not a value type itself — it
		// failed its own validation, mixes placements, or is `native. The first
		// two are already diagnosed on the parent, so name it rather than
		// repeating a rule the child does not actually break.
		if report {
			c.errorf(d.Pos(), "type %s inherits `value fields from %s, which is not a valid value type", d.Name, parent.Named.Obj().Name())
		}
		return false
	}
	if len(named.TypeParams()) > 0 {
		if report {
			c.errorf(d.Pos(),
				"value-type inheritance is not supported for generic types: %s has type parameters, so it cannot inherit from the value type %s",
				d.Name, parent.Named.Obj().Name())
		}
		return false
	}
	if len(parent.Named.TypeParams()) > 0 || len(parent.TypeArgs) > 0 {
		if report {
			c.errorf(d.Pos(),
				"value-type inheritance is not supported for generic types: the value parent %s has type parameters",
				parent.Named.Obj().Name())
		}
		return false
	}
	return c.markValueType(named, d, report)
}

// checkNoStrayValueFields is the backstop that keeps `check` and `build` in
// agreement (T1527): codegen's heap layout builder asserts that every field of a
// non-value type is instance-placed, so a type that reaches codegen with a
// `value field would panic there. Every path that produces one is diagnosed
// above; this sweep only fires when no real error was reported, since any sema
// error aborts the compile before codegen runs (warnings do not, so they must
// not disarm the backstop — hence hasErrors rather than len(c.errors)).
func (c *Checker) checkNoStrayValueFields(file *ast.File) {
	if c.hasErrors() {
		return
	}
	for _, decl := range file.Decls {
		if c.info.FilteredDecls[decl] {
			continue
		}
		d, ok := decl.(*ast.TypeDecl)
		if !ok || c.hasAnnotation(d.Annotations, "native") {
			continue
		}
		named := c.lookupNamedDecl(d)
		if named == nil || named.IsValueType() {
			continue
		}
		if !hasValuePlacedField(named.AllFields()) {
			continue
		}
		c.errorf(ownFieldPos(named, d),
			"type %s mixes `value` and instance fields; hybrid value+instance types are not yet supported (place all fields `value`, or none)", d.Name)
	}
}

// lookupNamedDecl resolves a TypeDecl to its Named type, or nil.
func (c *Checker) lookupNamedDecl(d *ast.TypeDecl) *types.Named {
	obj := c.scope.Lookup(d.Name)
	if obj == nil {
		return nil
	}
	tn, ok := obj.(*types.TypeName)
	if !ok {
		return nil
	}
	named, _ := tn.Type().(*types.Named)
	return named
}

// hasValuePlacedField reports whether any of the fields is `value-placed.
func hasValuePlacedField(fields []*types.Field) bool {
	for _, f := range fields {
		if f.Placement() == types.PlaceValue {
			return true
		}
	}
	return false
}

// valueTypeParent returns the first parent that is a value type, or nil.
func valueTypeParent(named *types.Named) *types.Named {
	for _, p := range named.Parents() {
		if p.Named.IsValueType() {
			return p.Named
		}
	}
	return nil
}

// ownFieldPos returns the position of the type's first own field, falling back
// to the declaration position when it has none.
func ownFieldPos(named *types.Named, d *ast.TypeDecl) ast.Pos {
	if len(named.Fields()) == 0 {
		return d.Pos()
	}
	p := named.Fields()[0].Pos()
	return ast.Pos{File: p.File, Line: p.Line, Column: p.Column}
}
