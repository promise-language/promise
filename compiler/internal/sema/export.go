package sema

import (
	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/types"
)

// ExportedScope creates a new scope containing only the `public symbols
// that were declared in the given file (not imported from other modules
// via `use X as _` glob imports). Used for module loading — the returned
// scope is what other modules see when they `use` this module.
func ExportedScope(info *Info, file *ast.File) *types.Scope {
	fileScope := info.Scopes[file]
	if fileScope == nil {
		return nil
	}
	exported := types.NewScope(nil, types.Pos{}, types.Pos{}, "module")
	for _, name := range fileScope.Names() {
		obj := fileScope.Lookup(name)
		if !isObjectExported(obj) {
			continue
		}
		// Skip symbols that were imported via glob import (`use X as _`).
		// A module should only re-export its own declarations, not symbols
		// it pulled in from dependencies.
		if info.GlobImportedObjs[obj] {
			continue
		}
		exported.Insert(obj)
	}
	return exported
}
