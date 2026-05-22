package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"djabi.dev/go/promise_lang/internal/sema"
	"djabi.dev/go/promise_lang/internal/types"
)

func TestMainFuncRe(t *testing.T) {
	match := []string{
		"main() {",
		"main() Type {",
		"main!() {",
		"main ! () {",
		"main!  () {",
	}
	noMatch := []string{
		"  main() {",
		"\tmain() {",
		"// main() {",
		"other_main() {",
	}
	for _, s := range match {
		if !mainFuncRe.MatchString(s) {
			t.Errorf("expected mainFuncRe to match %q", s)
		}
	}
	for _, s := range noMatch {
		if mainFuncRe.MatchString(s) {
			t.Errorf("expected mainFuncRe NOT to match %q", s)
		}
	}
}

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

func TestDiscoverMainFile(t *testing.T) {
	writeFile := func(t *testing.T, dir, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("toml_main_field_exists", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "promise.toml", "[module]\nname = \"app\"\nmain = \"entry.pr\"\n")
		writeFile(t, dir, "entry.pr", "main() {}\n")
		got, err := discoverMainFile(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := filepath.Join(dir, "entry.pr")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("toml_main_field_missing_file", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "promise.toml", "[module]\nname = \"app\"\nmain = \"missing.pr\"\n")
		_, err := discoverMainFile(dir)
		if err == nil {
			t.Fatal("expected error for missing main file")
		}
		if !strings.Contains(err.Error(), "missing.pr") {
			t.Errorf("error should mention filename, got: %v", err)
		}
	})

	t.Run("no_toml_one_regular_main", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "app.pr", "main() {\n  println(\"hi\");\n}\n")
		got, err := discoverMainFile(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := filepath.Join(dir, "app.pr")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("no_toml_one_failable_main", func(t *testing.T) {
		// T0423: failable main!() must be recognised as a valid entry point.
		dir := t.TempDir()
		writeFile(t, dir, "app.pr", "main!() {\n  println(\"hi\");\n}\n")
		got, err := discoverMainFile(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := filepath.Join(dir, "app.pr")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("no_toml_no_main", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "lib.pr", "helper() int { return 1; }\n")
		_, err := discoverMainFile(dir)
		if err == nil {
			t.Fatal("expected error when no main found")
		}
		if !strings.Contains(err.Error(), "no main()") {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("no_toml_multiple_mains", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "a.pr", "main() {}\n")
		writeFile(t, dir, "b.pr", "main() {}\n")
		_, err := discoverMainFile(dir)
		if err == nil {
			t.Fatal("expected error for multiple main files")
		}
		if !strings.Contains(err.Error(), "multiple files") {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("no_toml_skips_non_pr_files", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "app.go", "func main() {}\n")
		writeFile(t, dir, "app.pr", "main() {}\n")
		got, err := discoverMainFile(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := filepath.Join(dir, "app.pr")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("no_toml_skips_indented_main", func(t *testing.T) {
		dir := t.TempDir()
		// indented main() inside a method body — must NOT be recognised as entry point
		writeFile(t, dir, "lib.pr", "type Foo {\n  run() {\n    main();\n  }\n}\n")
		_, err := discoverMainFile(dir)
		if err == nil {
			t.Fatal("expected error: indented main() should not be an entry point")
		}
	})

	t.Run("nonexistent_directory", func(t *testing.T) {
		_, err := discoverMainFile("/tmp/does_not_exist_promise_test_xyz")
		if err == nil {
			t.Fatal("expected error for non-existent directory")
		}
		if !strings.Contains(err.Error(), "cannot read directory") {
			t.Errorf("unexpected error message: %v", err)
		}
	})
}
