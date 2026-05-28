package sema

import (
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// validateBuiltins runs after the define pass, before the check pass.
// It ensures that the .pr std files declared all operators, methods, and fields
// that codegen expects on builtin types. If a declaration is missing, the
// compiler errors instead of panicking during codegen.
func (c *Checker) validateBuiltins() {
	arithOps := []string{"+", "-", "*", "/", "%"}
	cmpOps := []string{"==", "!=", "<", ">", "<=", ">="}

	// Numeric types: arithmetic + comparison + unary negate
	numericTypes := map[string]*types.Named{
		"int":  types.TypInt,
		"i8":   types.TypI8,
		"i16":  types.TypI16,
		"i32":  types.TypI32,
		"i64":  types.TypI64,
		"uint": types.TypUint,
		"u8":   types.TypU8,
		"u16":  types.TypU16,
		"u32":  types.TypU32,
		"u64":  types.TypU64,
		"f32":  types.TypF32,
		"f64":  types.TypF64,
	}
	for name, nt := range numericTypes {
		for _, op := range arithOps {
			c.requireBinaryOp(name, nt, op)
		}
		for _, op := range cmpOps {
			c.requireBinaryOp(name, nt, op)
		}
		c.requireUnaryOp(name, nt, "-")
	}

	// Bool: logical + equality + unary not
	c.requireBinaryOp("bool", types.TypBool, "&&")
	c.requireBinaryOp("bool", types.TypBool, "||")
	c.requireBinaryOp("bool", types.TypBool, "==")
	c.requireBinaryOp("bool", types.TypBool, "!=")
	c.requireUnaryOp("bool", types.TypBool, "!")

	// String: concatenation + comparison
	c.requireBinaryOp("string", types.TypString, "+")
	for _, op := range cmpOps {
		c.requireBinaryOp("string", types.TypString, op)
	}

	// Char: comparison
	for _, op := range cmpOps {
		c.requireBinaryOp("char", types.TypChar, op)
	}

	// Iterator[T].next(), Stream[T].iter()
	c.requireMethod("Iterator", types.TypIter, "next")
	c.requireMethod("Stream", types.TypStream, "iter")

	// Range fields
	c.requireField("Range", types.TypRange, "start")
	c.requireField("Range", types.TypRange, "end")
	c.requireField("Range", types.TypRange, "inclusive")
}

// requireBinaryOp checks that a Named type has a method with the given name
// and exactly 1 parameter (binary operator).
func (c *Checker) requireBinaryOp(typeName string, named *types.Named, op string) {
	for _, m := range named.Methods() {
		if m.Name() == op && len(m.Sig().Params()) == 1 {
			return
		}
	}
	c.errorf(ast.Pos{}, "builtin type %s missing required binary operator %s", typeName, op)
}

// requireUnaryOp checks that a Named type has a method with the given name
// and 0 parameters (unary operator).
func (c *Checker) requireUnaryOp(typeName string, named *types.Named, op string) {
	for _, m := range named.Methods() {
		if m.Name() == op && len(m.Sig().Params()) == 0 {
			return
		}
	}
	c.errorf(ast.Pos{}, "builtin type %s missing required unary operator %s", typeName, op)
}

// requireMethod checks that a Named type has a method with the given name.
func (c *Checker) requireMethod(typeName string, named *types.Named, methodName string) {
	if named == nil {
		return // non-native universe type not yet populated (e.g., no std import)
	}
	if named.LookupMethod(methodName) == nil {
		c.errorf(ast.Pos{}, "builtin type %s missing required method %s", typeName, methodName)
	}
}

// requireField checks that a Named type has a field with the given name.
func (c *Checker) requireField(typeName string, named *types.Named, fieldName string) {
	if named == nil {
		return // non-native universe type not yet populated (e.g., no std import)
	}
	if named.LookupField(fieldName) == nil {
		c.errorf(ast.Pos{}, "builtin type %s missing required field %s", typeName, fieldName)
	}
}
