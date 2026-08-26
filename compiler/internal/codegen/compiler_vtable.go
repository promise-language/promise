package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// computeVtableInfo scans all type declarations and marks types that have children.
// A type has children if any other type inherits from it (directly or transitively).
func (c *Compiler) computeVtableInfo(file *ast.File) {
	for _, decl := range file.Decls {
		td, ok := decl.(*ast.TypeDecl)
		if !ok {
			continue
		}
		if c.info.FilteredDecls[decl] {
			continue // excluded by `target(cond) annotation for this build target
		}
		named := c.lookupNamedType(td.Name)
		if named == nil {
			continue
		}
		// T1160: record the inheritance edges of generic types too. hasChildren
		// deliberately skips them (a generic type is never itself a vtable receiver),
		// but a generic child of a concrete parent IS a possible runtime type of that
		// parent, so namedSubtreeMentionsSignature must be able to reach it.
		for _, pr := range named.Parents() {
			c.directChildren[pr.Named] = append(c.directChildren[pr.Named], named)
		}
		if len(named.TypeParams()) > 0 {
			continue
		}
		var markParents func(n *types.Named)
		markParents = func(n *types.Named) {
			for _, pr := range n.Parents() {
				c.hasChildren[pr.Named] = true
				markParents(pr.Named)
			}
		}
		markParents(named)
	}
}

// needsVtable reports whether a type needs virtual dispatch.
// True if the type has children (someone inherits from it) or is abstract.
// Value types are excluded: a value newtype (T1527) adds methods, never
// overrides, so a value parent that gains a child stays static-dispatch —
// value types carry no runtime type identity to dispatch on.
func (c *Compiler) needsVtable(named *types.Named) bool {
	return (c.hasChildren[named] || named.IsAbstract()) && !named.IsValueType()
}

// needsRttiDrop reports whether dropping a value of this static type must
// dispatch through RTTI (typeinfo.drop_fn_ptr) because the runtime type
// may differ. True for structural interfaces and polymorphic heap types
// (types with children or abstract). Excludes value types, copy types,
// and primitive scalars (none of which have runtime type identity).
func (c *Compiler) needsRttiDrop(named *types.Named) bool {
	if named.IsValueType() || named.IsCopy() || isPrimitiveScalar(named) {
		return false
	}
	return named.IsStructural() || c.needsVtable(named)
}

// isNativeTypeDecl checks if a type declaration has the `native annotation.
func isNativeTypeDecl(td *ast.TypeDecl) bool {
	for _, ann := range td.Annotations {
		if ann.Name == "native" {
			return true
		}
	}
	return false
}

// emitNativeOp dispatches a native operator to the LLVM instruction table.
// right is nil for unary operations.
func (c *Compiler) emitNativeOp(named *types.Named, op string, left, right value.Value) value.Value {
	cat := classify(named)
	if cat == CatUnknown {
		panic(fmt.Sprintf("codegen: native method %q on non-primitive type %s", op, named))
	}
	emitter := lookupNativeOp(cat, op)
	if emitter == nil {
		panic(fmt.Sprintf("codegen: no native emitter for %s.%s", named, op))
	}
	return emitter(c.block, left, right)
}
