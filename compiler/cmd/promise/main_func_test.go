package main

import (
	"testing"

	"djabi.dev/go/promise_lang/internal/sema"
	"djabi.dev/go/promise_lang/internal/types"
)

func TestHasMainFunc(t *testing.T) {
	var p types.Pos // zero-value position

	t.Run("empty_scope_order", func(t *testing.T) {
		info := &sema.Info{}
		if hasMainFunc(info) {
			t.Error("expected false for empty ScopeOrder")
		}
	})

	t.Run("scope_without_main", func(t *testing.T) {
		scope := types.NewScope(nil, p, p, "file")
		scope.Insert(types.NewFunc(p, "foo", nil))
		info := &sema.Info{ScopeOrder: []*types.Scope{scope}}
		if hasMainFunc(info) {
			t.Error("expected false when no main in scope")
		}
	})

	t.Run("main_is_type_not_func", func(t *testing.T) {
		scope := types.NewScope(nil, p, p, "file")
		scope.Insert(types.NewTypeName(p, "main", nil))
		info := &sema.Info{ScopeOrder: []*types.Scope{scope}}
		if hasMainFunc(info) {
			t.Error("expected false when main is a TypeName, not Func")
		}
	})

	t.Run("main_is_var_not_func", func(t *testing.T) {
		scope := types.NewScope(nil, p, p, "file")
		scope.Insert(types.NewVar(p, "main", nil))
		info := &sema.Info{ScopeOrder: []*types.Scope{scope}}
		if hasMainFunc(info) {
			t.Error("expected false when main is a Var, not Func")
		}
	})

	t.Run("main_func_present", func(t *testing.T) {
		scope := types.NewScope(nil, p, p, "file")
		scope.Insert(types.NewFunc(p, "main", nil))
		info := &sema.Info{ScopeOrder: []*types.Scope{scope}}
		if !hasMainFunc(info) {
			t.Error("expected true when main is a Func")
		}
	})

	t.Run("main_func_with_other_decls", func(t *testing.T) {
		scope := types.NewScope(nil, p, p, "file")
		scope.Insert(types.NewFunc(p, "helper", nil))
		scope.Insert(types.NewTypeName(p, "Foo", nil))
		scope.Insert(types.NewFunc(p, "main", nil))
		info := &sema.Info{ScopeOrder: []*types.Scope{scope}}
		if !hasMainFunc(info) {
			t.Error("expected true when main is among other decls")
		}
	})
}
