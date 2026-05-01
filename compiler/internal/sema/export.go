package sema

import (
	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/types"
)

// ExportedScope creates a new scope containing only the `public symbols
// from the given sema info's file scope. Used for module loading — the
// returned scope is what other modules see when they `use` this module.
func ExportedScope(info *Info, file *ast.File) *types.Scope {
	fileScope := info.Scopes[file]
	if fileScope == nil {
		return nil
	}
	exported := types.NewScope(nil, types.Pos{}, types.Pos{}, "module")
	for _, name := range fileScope.Names() {
		obj := fileScope.Lookup(name)
		if isObjectExported(obj) {
			exported.Insert(obj)
		}
	}
	return exported
}
