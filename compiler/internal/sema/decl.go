package sema

import (
	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/types"
)

// declare performs Pass 1: walk top-level declarations and insert names into file scope.
// This creates TypeName and Func objects so that forward references resolve.
func (c *Checker) declare(file *ast.File) {
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.TypeDecl:
			c.declareType(d)
		case *ast.EnumDecl:
			c.declareEnum(d)
		case *ast.FuncDecl:
			c.declareFunc(d)
		}
	}
}

func (c *Checker) declareType(d *ast.TypeDecl) {
	tn := types.NewTypeName(tpos(d.Pos()), d.Name, nil)
	if !c.insert(tn) {
		return
	}
	tparams := c.declareTypeParams(d.TypeParams)
	types.NewNamed(tn, tparams)
}

func (c *Checker) declareEnum(d *ast.EnumDecl) {
	tn := types.NewTypeName(tpos(d.Pos()), d.Name, nil)
	if !c.insert(tn) {
		return
	}
	tparams := c.declareTypeParams(d.TypeParams)
	types.NewEnum(tn, tparams)
}

func (c *Checker) declareFunc(d *ast.FuncDecl) {
	fn := types.NewFunc(tpos(d.Pos()), d.Name, nil)
	c.insert(fn)
}

// declareTypeParams creates TypeParam objects from AST type parameters.
// These are not inserted into any scope yet — that happens in define when needed.
func (c *Checker) declareTypeParams(astParams []*ast.TypeParam) []*types.TypeParam {
	if len(astParams) == 0 {
		return nil
	}
	result := make([]*types.TypeParam, len(astParams))
	for i, ap := range astParams {
		tn := types.NewTypeName(tpos(ap.Pos()), ap.Name, nil)
		// Constraints are resolved later in define pass
		result[i] = types.NewTypeParam(tn, nil, i)
	}
	return result
}

// define performs Pass 2: resolve type structures, populate fields/methods/variants.
func (c *Checker) define(file *ast.File) {
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.TypeDecl:
			c.defineType(d)
		case *ast.EnumDecl:
			c.defineEnum(d)
		case *ast.FuncDecl:
			c.defineFunc(d)
		}
	}
}

func (c *Checker) defineType(d *ast.TypeDecl) {
	obj := c.scope.Lookup(d.Name)
	if obj == nil {
		return // error in declare
	}
	tn, ok := obj.(*types.TypeName)
	if !ok {
		return
	}
	named, ok := tn.Type().(*types.Named)
	if !ok {
		return
	}

	// Open type-params scope if generic
	if len(named.TypeParams()) > 0 {
		c.openScope(d, "typeparams:"+d.Name)
		for _, tp := range named.TypeParams() {
			c.insert(tp.Obj())
		}
		// Resolve constraints now that params are in scope
		c.resolveTypeParamConstraints(d.TypeParams, named.TypeParams())
		defer c.closeScope()
	}

	// Resolve parent types (is clauses)
	for _, inh := range d.Inherits {
		pt := c.resolveType(inh)
		if pt == nil {
			continue
		}
		pn, ok := pt.(*types.Named)
		if !ok {
			c.errorf(inh.Pos(), "parent type must be a named type, got %s", pt)
			continue
		}
		named.AddParent(pn)
	}

	// Resolve fields
	for _, fd := range d.Fields {
		c.defineField(named, fd)
	}

	// Resolve methods
	for _, md := range d.Methods {
		c.defineMethod(named, md, d.Name)
	}
}

func (c *Checker) defineField(named *types.Named, fd *ast.FieldDecl) {
	typ := c.resolveType(fd.Type)
	if typ == nil {
		return
	}
	placement := c.resolvePlacement(fd.Annotations)
	isRaw := c.hasAnnotation(fd.Annotations, "raw")
	hasDef := fd.Default != nil

	named.AddField(types.NewField(tpos(fd.Pos()), fd.Name, typ, placement, isRaw, hasDef))
}

func (c *Checker) defineMethod(named *types.Named, md *ast.MethodDecl, typeName string) {
	sig := c.resolveMethodSignature(named, md)
	if sig == nil {
		return
	}

	placement := c.resolvePlacement(md.Annotations)
	abstract := c.hasAnnotation(md.Annotations, "abstract")
	native := c.hasAnnotation(md.Annotations, "native")

	// Validate: abstract/native methods must not have a body
	if abstract && md.Body != nil {
		c.errorf(md.Pos(), "abstract method %s.%s must not have a body", typeName, md.Name)
	}
	if native && md.Body != nil {
		c.errorf(md.Pos(), "native method %s.%s must not have a body", typeName, md.Name)
	}
	// Non-abstract, non-native methods must have a body
	if !abstract && !native && md.Body == nil {
		c.errorf(md.Pos(), "method %s.%s must have a body (or be marked `abstract or `native)", typeName, md.Name)
	}

	named.AddMethod(types.NewMethod(tpos(md.Pos()), md.Name, sig, placement, abstract, native))
}

func (c *Checker) resolveMethodSignature(named *types.Named, md *ast.MethodDecl) *types.Signature {
	// Resolve receiver
	var recv *types.Param
	if md.Receiver != nil {
		ref := resolveRefMod(md.Receiver.RefMod)
		recv = types.NewParam("this", named, ref)
	} else {
		// Methods without explicit receiver default to value receiver
		recv = types.NewParam("this", named, types.RefNone)
	}

	// Resolve parameters
	params := make([]*types.Param, len(md.Params))
	for i, p := range md.Params {
		pt := c.resolveType(p.Type)
		if pt == nil {
			return nil
		}
		params[i] = types.NewParam(p.Name, pt, resolveRefMod(p.RefMod))
	}

	// Resolve return type
	var result types.Type
	var canError bool
	if md.ReturnType != nil {
		result = c.resolveType(md.ReturnType.Type)
		if result == nil {
			return nil
		}
		canError = md.ReturnType.CanError
	}

	return types.NewSignature(recv, params, result, canError)
}

func (c *Checker) defineEnum(d *ast.EnumDecl) {
	obj := c.scope.Lookup(d.Name)
	if obj == nil {
		return
	}
	tn, ok := obj.(*types.TypeName)
	if !ok {
		return
	}
	enum, ok := tn.Type().(*types.Enum)
	if !ok {
		return
	}

	// Open type-params scope if generic
	if len(enum.TypeParams()) > 0 {
		c.openScope(d, "typeparams:"+d.Name)
		for _, tp := range enum.TypeParams() {
			c.insert(tp.Obj())
		}
		c.resolveTypeParamConstraints(d.TypeParams, enum.TypeParams())
		defer c.closeScope()
	}

	// Resolve variants
	for _, v := range d.Variants {
		fields := make([]*types.VarField, len(v.Fields))
		for i, f := range v.Fields {
			ft := c.resolveType(f.Type)
			if ft == nil {
				ft = types.TypVoid // fallback to avoid nil
			}
			fields[i] = types.NewVarField(f.Name, ft)
		}
		enum.AddVariant(types.NewVariant(v.Name, fields))
	}
}

func (c *Checker) defineFunc(d *ast.FuncDecl) {
	obj := c.scope.Lookup(d.Name)
	if obj == nil {
		return
	}
	fn, ok := obj.(*types.Func)
	if !ok {
		return
	}

	// Open type-params scope if generic
	if len(d.TypeParams) > 0 {
		c.openScope(d, "typeparams:"+d.Name)
		// Create type param objects
		for i, tp := range d.TypeParams {
			tn := types.NewTypeName(tpos(tp.Pos()), tp.Name, nil)
			types.NewTypeParam(tn, nil, i)
			c.insert(tn)
		}
		defer c.closeScope()
	}

	sig := c.resolveFuncSignature(d)
	if sig != nil {
		fn.SetType(sig)
	}
}

func (c *Checker) resolveFuncSignature(d *ast.FuncDecl) *types.Signature {
	// Resolve parameters
	params := make([]*types.Param, len(d.Params))
	for i, p := range d.Params {
		pt := c.resolveType(p.Type)
		if pt == nil {
			return nil
		}
		params[i] = types.NewParam(p.Name, pt, resolveRefMod(p.RefMod))
	}

	// Resolve return type
	var result types.Type
	var canError bool
	if d.ReturnType != nil {
		result = c.resolveType(d.ReturnType.Type)
		if result == nil {
			return nil
		}
		canError = d.ReturnType.CanError
	}

	return types.NewSignature(nil, params, result, canError)
}

// resolveTypeParamConstraints resolves constraints for type parameters.
func (c *Checker) resolveTypeParamConstraints(astParams []*ast.TypeParam, tparams []*types.TypeParam) {
	for i, ap := range astParams {
		if len(ap.Constraint) == 0 {
			continue
		}
		// For now, use the first constraint only (single constraint support)
		ct := c.resolveType(ap.Constraint[0])
		if ct != nil {
			tparams[i].SetConstraint(ct)
		}
	}
}

// resolvePlacement extracts placement from meta annotations.
func (c *Checker) resolvePlacement(annotations []*ast.MetaAnnotation) types.Placement {
	for _, ann := range annotations {
		switch ann.Name {
		case "value":
			return types.PlaceValue
		case "variant":
			return types.PlaceVariant
		case "type":
			return types.PlaceType
		case "instance":
			return types.PlaceInstance
		}
	}
	return types.PlaceInstance
}

// hasAnnotation checks if a specific annotation is present.
func (c *Checker) hasAnnotation(annotations []*ast.MetaAnnotation, name string) bool {
	for _, ann := range annotations {
		if ann.Name == name {
			return true
		}
	}
	return false
}
