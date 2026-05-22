package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/ownership"
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

func TestDiscoverProject(t *testing.T) {
	writeFile := func(t *testing.T, dir, name, content string) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("toml_with_multiple_files", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "promise.toml", "[module]\nname = \"myapp\"\nepoch = \"2026.0\"\n")
		writeFile(t, dir, "main.pr", "main() {}\n")
		writeFile(t, dir, "helper.pr", "type Helper { int x; }\n")
		cfg, files, err := discoverProject(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg == nil {
			t.Fatal("expected config, got nil")
		}
		if cfg.Name != "myapp" {
			t.Errorf("got name %q, want %q", cfg.Name, "myapp")
		}
		if len(files) != 2 {
			t.Errorf("got %d files, want 2: %v", len(files), files)
		}
	})

	t.Run("toml_excludes_test_files", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "promise.toml", "[module]\nname = \"myapp\"\nepoch = \"2026.0\"\n")
		writeFile(t, dir, "main.pr", "main() {}\n")
		writeFile(t, dir, "main_test.pr", "test_foo() `test {}\n")
		_, files, err := discoverProject(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(files) != 1 {
			t.Errorf("got %d files, want 1 (test files should be excluded): %v", len(files), files)
		}
	})

	t.Run("toml_only_no_pr_files", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "promise.toml", "[module]\nname = \"empty\"\nepoch = \"2026.0\"\n")
		_, _, err := discoverProject(dir)
		if err == nil {
			t.Fatal("expected error for project with no .pr files")
		}
		if !strings.Contains(err.Error(), "no .pr files") {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("no_toml_returns_nil", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "main.pr", "main() {}\n")
		cfg, files, err := discoverProject(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg != nil {
			t.Errorf("expected nil cfg, got %v", cfg)
		}
		if files != nil {
			t.Errorf("expected nil files, got %v", files)
		}
	})

	t.Run("nested_module_excluded", func(t *testing.T) {
		// Subdirectory with its own promise.toml is treated as a nested
		// module and excluded from the parent project's source list.
		dir := t.TempDir()
		writeFile(t, dir, "promise.toml", "[module]\nname = \"outer\"\nepoch = \"2026.0\"\n")
		writeFile(t, dir, "main.pr", "main() {}\n")
		writeFile(t, dir, "inner/promise.toml", "[module]\nname = \"inner\"\nepoch = \"2026.0\"\n")
		writeFile(t, dir, "inner/lib.pr", "type Inner {}\n")
		_, files, err := discoverProject(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(files) != 1 {
			t.Errorf("got %d files, want 1 (nested module should be excluded): %v", len(files), files)
		}
		for _, f := range files {
			if strings.Contains(f, "inner/") || strings.Contains(f, string(filepath.Separator)+"inner"+string(filepath.Separator)) {
				t.Errorf("nested module file should not be included: %s", f)
			}
		}
	})

	t.Run("invalid_toml", func(t *testing.T) {
		dir := t.TempDir()
		// Missing required [module] name
		writeFile(t, dir, "promise.toml", "[module]\nepoch = \"2026.0\"\n")
		writeFile(t, dir, "main.pr", "main() {}\n")
		_, _, err := discoverProject(dir)
		if err == nil {
			t.Fatal("expected error for invalid promise.toml")
		}
	})
}

// TestPrintFileErrorsMultiFile verifies that printFileErrors loads source
// context from the file referenced by each error's pos.File, not from the
// fallback filename. This matters for project / module-test builds where
// errors can come from any of several merged source files.
func TestPrintFileErrorsMultiFile(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.pr")
	helperPath := filepath.Join(dir, "helper.pr")
	if err := os.WriteFile(mainPath, []byte("main() {}\n// main file line 2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(helperPath, []byte("type Helper {}\n// helper file UNIQUE_HELPER_LINE\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Reset the package-level cache between test runs so we read fresh content.
	fileLineCache = map[string][]string{}

	semaErr := &sema.Error{
		Pos: ast.Pos{File: helperPath, Line: 2, Column: 0},
		Msg: "synthetic helper error",
	}
	output := captureStderr(func() {
		printFileErrors(mainPath, []error{semaErr})
	})

	// Header line should reference helper.pr.
	if !strings.Contains(output, helperPath+":2:0:") {
		t.Errorf("expected error header to reference %s:2:0, got:\n%s", helperPath, output)
	}
	// Context should be loaded from helper.pr (contains UNIQUE_HELPER_LINE),
	// not from main.pr (which doesn't have that string).
	if !strings.Contains(output, "UNIQUE_HELPER_LINE") {
		t.Errorf("expected output to include source context from helper.pr, got:\n%s", output)
	}
}

// TestPrintFileErrorsFallbackToFilename verifies the fallback path: when an
// error's Pos.File is empty, context loading falls back to the function's
// filename argument.
func TestPrintFileErrorsFallbackToFilename(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.pr")
	if err := os.WriteFile(mainPath, []byte("line 1\nFALLBACK_MARKER line 2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	fileLineCache = map[string][]string{}

	semaErr := &sema.Error{
		// Empty Pos.File should fall back to filename arg.
		Pos: ast.Pos{File: "", Line: 2, Column: 0},
		Msg: "synthetic error",
	}
	output := captureStderr(func() {
		printFileErrors(mainPath, []error{semaErr})
	})
	if !strings.Contains(output, "FALLBACK_MARKER") {
		t.Errorf("expected fallback to load context from %s, got:\n%s", mainPath, output)
	}
}

// TestPrintFileErrorsOwnershipError verifies *ownership.Error is handled
// alongside *sema.Error.
func TestPrintFileErrorsOwnershipError(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.pr")
	if err := os.WriteFile(srcPath, []byte("OWNED_LINE\n"), 0644); err != nil {
		t.Fatal(err)
	}
	fileLineCache = map[string][]string{}

	ownErr := &ownership.Error{
		Pos: ast.Pos{File: srcPath, Line: 1, Column: 0},
		Msg: "synthetic ownership error",
	}
	output := captureStderr(func() {
		printFileErrors(srcPath, []error{ownErr})
	})
	if !strings.Contains(output, "synthetic ownership error") {
		t.Errorf("expected ownership error message, got:\n%s", output)
	}
	if !strings.Contains(output, "OWNED_LINE") {
		t.Errorf("expected source context, got:\n%s", output)
	}
}

// TestPrintFileErrorsPlainError verifies plain (non-positional) errors are
// printed with no source context, just the message.
func TestPrintFileErrorsPlainError(t *testing.T) {
	fileLineCache = map[string][]string{}
	plainErr := errors.New("top-level diagnostic with no position")
	output := captureStderr(func() {
		printFileErrors("/path/that/does/not/exist", []error{plainErr})
	})
	if !strings.Contains(output, "top-level diagnostic with no position") {
		t.Errorf("expected plain error message, got:\n%s", output)
	}
	// No source context should appear since there's no readable file.
	if strings.Contains(output, "  > ") {
		t.Errorf("did not expect source context for plain error, got:\n%s", output)
	}
}

// TestCompileProjectFrontendSuccess verifies the success path of
// compileProjectFrontend: multi-file project parses, merges, and runs
// through sema/ownership without errors. The error paths call os.Exit(1)
// and are exercised via the subprocess integration tests in
// build_project_test.go.
func TestCompileProjectFrontendSuccess(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"),
		[]byte("[module]\nname = \"app\"\nepoch = \"2026.0\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.pr"),
		[]byte("main() { h := Helper(value: 99); }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "helper.pr"),
		[]byte("type Helper { int value; }\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, files, err := discoverProject(dir)
	if err != nil {
		t.Fatalf("discoverProject failed: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected cfg, got nil")
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d: %v", len(files), files)
	}

	file, info := compileProjectFrontend(cfg.Dir, files, "")
	if file == nil {
		t.Fatal("expected merged ast.File, got nil")
	}
	if info == nil {
		t.Fatal("expected sema.Info, got nil")
	}

	// Symbol from helper.pr should be visible after the merge: walk the
	// merged file's declarations to confirm Helper resolved.
	var foundHelper, foundMain bool
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.TypeDecl:
			if d.Name == "Helper" {
				foundHelper = true
			}
		case *ast.FuncDecl:
			if d.Name == "main" {
				foundMain = true
			}
		}
	}
	if !foundHelper {
		t.Error("expected Helper TypeDecl in merged AST")
	}
	if !foundMain {
		t.Error("expected main FuncDecl in merged AST")
	}
}
