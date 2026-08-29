package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/ownership"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
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

func TestDiscoverProject(t *testing.T) {
	t.Parallel()
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

// TestResolveTarget exercises the shared file-vs-project policy (T1603) directly,
// branch by branch. The integration tests in build_project_test.go drive the same
// logic through a subprocess (real end-to-end behavior) but don't instrument the
// in-process function; this unit test covers every path — including the ones the
// subprocess tests can't reach cheaply (the discoverProject error path, the
// standalone-file success path, and the empty-arg → CWD default).
func TestResolveTarget(t *testing.T) {
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

	t.Run("directory_is_project", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "promise.toml", "[module]\nname = \"proj\"\nepoch = \"2026.0\"\n")
		writeFile(t, dir, "main.pr", "main() {}\n")
		cfg, files, file, err := resolveTarget(dir, "build")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg == nil {
			t.Fatal("expected project cfg, got nil")
		}
		if cfg.Name != "proj" {
			t.Errorf("got cfg.Name %q, want %q", cfg.Name, "proj")
		}
		if len(files) != 1 {
			t.Errorf("got %d files, want 1: %v", len(files), files)
		}
		if file != "" {
			t.Errorf("expected empty file for a project target, got %q", file)
		}
	})

	t.Run("empty_arg_defaults_to_cwd_project", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "promise.toml", "[module]\nname = \"cwdproj\"\nepoch = \"2026.0\"\n")
		writeFile(t, dir, "main.pr", "main() {}\n")
		t.Chdir(dir)
		cfg, _, file, err := resolveTarget("", "run")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg == nil {
			t.Fatal("expected empty arg to resolve CWD as a project, got nil cfg")
		}
		if cfg.Name != "cwdproj" {
			t.Errorf("got cfg.Name %q, want %q", cfg.Name, "cwdproj")
		}
		if file != "" {
			t.Errorf("expected empty file for a project target, got %q", file)
		}
	})

	t.Run("directory_without_promise_toml_is_error", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "main.pr", "main() {}\n")
		_, _, _, err := resolveTarget(dir, "run")
		if err == nil {
			t.Fatal("expected non-project directory to be an error")
		}
		if !strings.Contains(err.Error(), "is not a Promise project") {
			t.Errorf("unexpected error message: %v", err)
		}
		// The hint must name the command actually typed, not a hard-coded one.
		if !strings.Contains(err.Error(), "promise run file.pr") {
			t.Errorf("hint should name the 'run' command, got: %v", err)
		}
	})

	t.Run("directory_project_with_no_pr_files_propagates_error", func(t *testing.T) {
		// discoverProject returns an error (not nil cfg) for a promise.toml with no
		// source files; resolveTarget must surface it rather than mislabel the dir.
		dir := t.TempDir()
		writeFile(t, dir, "promise.toml", "[module]\nname = \"empty\"\nepoch = \"2026.0\"\n")
		_, _, _, err := resolveTarget(dir, "build")
		if err == nil {
			t.Fatal("expected error for a project with no .pr files")
		}
		if !strings.Contains(err.Error(), "no .pr files") {
			t.Errorf("expected the discoverProject error to propagate, got: %v", err)
		}
	})

	t.Run("file_inside_project_is_error", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "promise.toml", "[module]\nname = \"inproj\"\nepoch = \"2026.0\"\n")
		writeFile(t, dir, "main.pr", "main() {}\n")
		_, _, _, err := resolveTarget(filepath.Join(dir, "main.pr"), "emit-ir")
		if err == nil {
			t.Fatal("expected a file inside a project to be an error")
		}
		if !strings.Contains(err.Error(), "belongs to the project at") {
			t.Errorf("unexpected error message: %v", err)
		}
		if !strings.Contains(err.Error(), "hint: emit-ir the project directly:") {
			t.Errorf("hint should name the 'emit-ir' command, got: %v", err)
		}
	})

	t.Run("file_in_project_subdir_is_error", func(t *testing.T) {
		// The enclosing project is an ancestor, not the file's own directory —
		// exercises the walk-up in findEnclosingProjectDir.
		dir := t.TempDir()
		writeFile(t, dir, "promise.toml", "[module]\nname = \"subproj\"\nepoch = \"2026.0\"\n")
		writeFile(t, dir, "src/main.pr", "main() {}\n")
		_, _, _, err := resolveTarget(filepath.Join(dir, "src", "main.pr"), "build")
		if err == nil {
			t.Fatal("expected a file in a project subdir to be an error")
		}
		if !strings.Contains(err.Error(), "belongs to the project at") {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("standalone_file_resolves_to_single_file", func(t *testing.T) {
		dir := t.TempDir()
		// No promise.toml anywhere above (t.TempDir is under the OS temp root).
		writeFile(t, dir, "solo.pr", "main() {}\n")
		path := filepath.Join(dir, "solo.pr")
		cfg, files, file, err := resolveTarget(path, "build")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg != nil {
			t.Errorf("expected nil cfg for a standalone file, got %v", cfg)
		}
		if files != nil {
			t.Errorf("expected nil files for a standalone file, got %v", files)
		}
		if file != path {
			t.Errorf("got file %q, want %q", file, path)
		}
	})

	t.Run("nonexistent_path_returned_as_file", func(t *testing.T) {
		// A name that doesn't exist is handed back verbatim as a file so the
		// frontend reports a clean file-not-found error — resolveTarget must not
		// claim project membership or error out itself.
		missing := filepath.Join(t.TempDir(), "does_not_exist.pr")
		cfg, files, file, err := resolveTarget(missing, "run")
		if err != nil {
			t.Fatalf("nonexistent path should not error from resolveTarget, got: %v", err)
		}
		if cfg != nil || files != nil {
			t.Errorf("expected nil cfg/files for a nonexistent path, got cfg=%v files=%v", cfg, files)
		}
		if file != missing {
			t.Errorf("got file %q, want the original arg %q", file, missing)
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
