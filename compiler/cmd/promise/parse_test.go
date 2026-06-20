package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antlr4-go/antlr/v4"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/module"
	"github.com/promise-language/promise/compiler/internal/parser"
	"github.com/promise-language/promise/compiler/internal/sema"
)

// testdataDir resolves the testdata directory relative to the project root.
func testdataDir(t *testing.T) string {
	t.Helper()
	// cmd/promise/ is two levels below the project root where testdata/ lives.
	dir, err := filepath.Abs("../../testdata")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// parseFile parses a .pr file and returns the number of errors.
func parseFile(path string) int {
	input, err := antlr.NewFileStream(path)
	if err != nil {
		return -1
	}

	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	lexEl := &errorCounter{}
	lexer.AddErrorListener(lexEl)

	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := parser.NewPromiseParser(stream)
	p.RemoveErrorListeners()
	parseEl := &errorCounter{}
	p.AddErrorListener(parseEl)

	p.CompilationUnit()

	return lexEl.errors + parseEl.errors
}

// parseString parses an inline string and returns the number of errors.
func parseString(source string) int {
	input := antlr.NewInputStream(source)

	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	lexEl := &errorCounter{}
	lexer.AddErrorListener(lexEl)

	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := parser.NewPromiseParser(stream)
	p.RemoveErrorListeners()
	parseEl := &errorCounter{}
	p.AddErrorListener(parseEl)

	p.CompilationUnit()

	return lexEl.errors + parseEl.errors
}

type errorCounter struct {
	antlr.DefaultErrorListener
	errors int
}

func (l *errorCounter) SyntaxError(
	_ antlr.Recognizer, _ interface{}, _, _ int, _ string, _ antlr.RecognitionException,
) {
	l.errors++
}

// TestParseValidFiles walks testdata/valid/ and verifies each .pr file parses without errors.
func TestParseValidFiles(t *testing.T) {
	dir := filepath.Join(testdataDir(t), "valid")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".pr" {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			path := filepath.Join(dir, e.Name())
			errs := parseFile(path)
			if errs != 0 {
				t.Errorf("expected 0 errors, got %d", errs)
			}
		})
	}
}

// TestParseInvalidFiles walks testdata/invalid/ and verifies each .pr file produces errors.
func TestParseInvalidFiles(t *testing.T) {
	dir := filepath.Join(testdataDir(t), "invalid")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".pr" {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			path := filepath.Join(dir, e.Name())
			errs := parseFile(path)
			if errs == 0 {
				t.Error("expected parse errors but got none")
			}
		})
	}
}

// TestParseRootFiles verifies the top-level testdata files parse cleanly.
func TestParseRootFiles(t *testing.T) {
	rootFiles := []string{"hello.pr", "features.pr", "comprehensive.pr"}
	dir := testdataDir(t)
	for _, name := range rootFiles {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name)
			errs := parseFile(path)
			if errs != 0 {
				t.Errorf("expected 0 errors, got %d", errs)
			}
		})
	}
}

// TestExpressionPrecedence verifies specific precedence scenarios with inline parsing.
func TestExpressionPrecedence(t *testing.T) {
	cases := []struct {
		name string
		code string
	}{
		{"postfix_panic_plus", `main() { Int x = f()?! + 1; }`},
		{"postfix_propagate_plus", `main() { Int x = f()?^ + 1; }`},
		{"optional_chain_plus", `main() { Float x = a?.b + 1.0; }`},
		{"member_call_chain", `main() { x.y().z.w(); }`},
		{"nested_index", `main() { Int x = a[b[0]]; }`},
		{"unary_neg_mul", `main() { Int x = -a * b; }`},
		{"comparison_chain", `main() { Bool x = a > b && c < d; }`},
		{"elvis_or", `main() { Int x = a ?: b || c; }`},
		{"range_add", `main() { r := (a + 1)..(b + 2); }`},
		{"receive_member", `main() { Int x = <-ch; }`},
		{"is_and", `main() { Bool x = a is Foo && b is Bar; }`},
		{"as_bang", `main() { Foo x = a as! Foo; }`},
		{"error_handler", `main() { Int x = f() ? e { return 0; }; }`},
		{"error_handler_discard", `main() { Int x = f() ? _ { return 0; }; }`},
		{"mixed_postfix", `main() { x.foo()?!.bar()?.baz; }`},
		{"nested_call", `main() { f(g(h())); }`},
		{"index_call", `main() { f()[0].bar(); }`},
		{"inclusive_range", `main() { for x in 0..=10 {} }`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := parseString(tc.code)
			if errs != 0 {
				t.Errorf("expected 0 errors, got %d for: %s", errs, tc.code)
			}
		})
	}
}

// TestLambdaSyntax verifies all lambda forms parse correctly.
func TestLambdaSyntax(t *testing.T) {
	cases := []struct {
		name string
		code string
	}{
		{"single_untyped", `main() { f := |x| -> x + 1; }`},
		{"multi_untyped", `main() { f := |a, b| -> a + b; }`},
		{"typed_params", `main() { f := |Int a, Int b| -> a + b; }`},
		{"block_body", `main() { f := |x| { return x; }; }`},
		{"no_params_block", `main() { f := || { return 42; }; }`},
		{"no_params_expr", `main() { f := || -> 42; }`},
		{"move_capture", `main() { f := move |x| -> x; }`},
		{"discard_param", `main() { f := |_| -> 0; }`},
		{"typed_return_block", `main() { f := |Int x| -> Int { return x; }; }`},
		{"ref_param", `main() { f := |String s| -> s.len(); }`},
		{"chained_lambdas", `main() { a.map(|x| -> x * 2).filter(|x| -> x > 0); }`},
		{"lambda_as_arg", `main() { run(|| { io.print_line("hi"); }); }`},
		{"nested_lambda", `main() { f := |x| -> |y| -> x + y; }`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := parseString(tc.code)
			if errs != 0 {
				t.Errorf("expected 0 errors, got %d for: %s", errs, tc.code)
			}
		})
	}
}

// TestNumericLiterals verifies valid and invalid numeric forms.
func TestNumericLiterals(t *testing.T) {
	valid := []struct {
		name string
		code string
	}{
		{"zero", `main() { Int x = 0; }`},
		{"decimal", `main() { Int x = 42; }`},
		{"underscore", `main() { Int x = 1_000_000; }`},
		{"hex", `main() { Int x = 0xFF; }`},
		{"hex_upper", `main() { Int x = 0XAB; }`},
		{"hex_underscore", `main() { Int x = 0xFF_FF; }`},
		{"octal", `main() { Int x = 0o77; }`},
		{"binary", `main() { Int x = 0b1010; }`},
		{"binary_underscore", `main() { Int x = 0b1111_0000; }`},
		{"float_simple", `main() { Float x = 3.14; }`},
		{"float_exp", `main() { Float x = 1e5; }`},
		{"float_exp_neg", `main() { Float x = 2.5e-3; }`},
		{"float_exp_pos", `main() { Float x = 1.0e+10; }`},
		{"float_zero", `main() { Float x = 0.0; }`},
		{"float_underscore", `main() { Float x = 1_000.25; }`},
	}
	for _, tc := range valid {
		t.Run("valid/"+tc.name, func(t *testing.T) {
			errs := parseString(tc.code)
			if errs != 0 {
				t.Errorf("expected 0 errors, got %d for: %s", errs, tc.code)
			}
		})
	}

	invalid := []struct {
		name string
		code string
	}{
		{"leading_zero_int", `main() { Int x = 01; }`},
		{"leading_zeros_int", `main() { Int x = 0123; }`},
		{"leading_zero_float", `main() { Float x = 00.5; }`},
		{"leading_zeros_float", `main() { Float x = 001.0; }`},
	}
	for _, tc := range invalid {
		t.Run("invalid/"+tc.name, func(t *testing.T) {
			errs := parseString(tc.code)
			if errs == 0 {
				t.Errorf("expected errors for: %s", tc.code)
			}
		})
	}
}

// TestStringLiterals verifies all string literal forms.
func TestStringLiterals(t *testing.T) {
	cases := []struct {
		name string
		code string
	}{
		{"simple", `main() { String s = "hello"; }`},
		{"escape_n", `main() { String s = "a\n"; }`},
		{"escape_t", `main() { String s = "a\t"; }`},
		{"escape_r", `main() { String s = "a\r"; }`},
		{"escape_backslash", `main() { String s = "a\\b"; }`},
		{"escape_quote", `main() { String s = "a\"b"; }`},
		{"escape_null", `main() { String s = "a\0b"; }`},
		{"escape_brace", `main() { String s = "a\{b"; }`},
		{"interpolation", `main() { String s = "hello {name}"; }`},
		{"interp_expr", `main() { String s = "{a + b}"; }`},
		{"multi_interp", `main() { String s = "{a} and {b}"; }`},
		{"raw_string", `main() { String s = r"C:\path\to"; }`},
		{"raw_empty", `main() { String s = r""; }`},
		{"empty_string", `main() { String s = ""; }`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := parseString(tc.code)
			if errs != 0 {
				t.Errorf("expected 0 errors, got %d for: %s", errs, tc.code)
			}
		})
	}
}

// TestCharLiterals verifies valid and invalid char literal forms.
func TestCharLiterals(t *testing.T) {
	valid := []struct {
		name string
		code string
	}{
		{"plain", `main() { Char c = 'a'; }`},
		{"digit", `main() { Char c = '0'; }`},
		{"space", `main() { Char c = ' '; }`},
		{"escape_n", `main() { Char c = '\n'; }`},
		{"escape_t", `main() { Char c = '\t'; }`},
		{"escape_r", `main() { Char c = '\r'; }`},
		{"escape_b", `main() { Char c = '\b'; }`},
		{"escape_backslash", `main() { Char c = '\\'; }`},
		{"escape_quote", `main() { Char c = '\''; }`},
		{"escape_null", `main() { Char c = '\0'; }`},
	}
	for _, tc := range valid {
		t.Run("valid/"+tc.name, func(t *testing.T) {
			errs := parseString(tc.code)
			if errs != 0 {
				t.Errorf("expected 0 errors, got %d for: %s", errs, tc.code)
			}
		})
	}

	invalid := []struct {
		name string
		code string
	}{
		{"hex_escape", `main() { Char c = '\x41'; }`},
		{"unicode_escape", `main() { Char c = '\u0041'; }`},
		{"unknown_escape", `main() { Char c = '\a'; }`},
	}
	for _, tc := range invalid {
		t.Run("invalid/"+tc.name, func(t *testing.T) {
			errs := parseString(tc.code)
			if errs == 0 {
				t.Errorf("expected errors for: %s", tc.code)
			}
		})
	}
}

// TestControlFlow verifies all control flow constructs.
func TestControlFlow(t *testing.T) {
	cases := []struct {
		name string
		code string
	}{
		{"if_simple", `main() { if x > 0 { y(); } }`},
		{"if_else", `main() { if x > 0 { y(); } else { z(); } }`},
		{"if_else_if", `main() { if x > 0 { a(); } else if x < 0 { b(); } else { c(); } }`},
		{"if_unwrap", `main() { if val := maybe { consume(val); } }`},
		{"if_unwrap_discard", `main() { if _ := maybe { ok(); } }`},
		{"for_in", `main() { for x in items { consume(x); } }`},
		{"for_in_index", `main() { for i, x in items { consume(i, x); } }`},
		{"for_in_discard_index", `main() { for _, x in items { consume(x); } }`},
		{"for_in_discard_value", `main() { for i, _ in items { consume(i); } }`},
		{"for_in_range", `main() { for x in 0..10 { consume(x); } }`},
		{"for_in_range_incl", `main() { for x in 0..=9 { consume(x); } }`},
		{"for_classic_typed", `main() { for Int i = 0; i < 10; i += 1 { consume(i); } }`},
		{"for_classic_inferred", `main() { for i := 0; i < 10; i += 1 { consume(i); } }`},
		{"for_classic_expr_update", `main() { for i := 0; i < 10; inc(i) { consume(i); } }`},
		{"for_infinite", `main() { for { break; } }`},
		{"while_simple", `main() { while x < 10 { x += 1; } }`},
		{"while_unwrap", `main() { while val := next() { consume(val); } }`},
		{"break_continue", `main() { for x in items { if x > 0 { continue; } break; } }`},
		{"nested_loops", `main() { for i in 0..10 { for j in 0..10 { consume(i, j); } } }`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := parseString(tc.code)
			if errs != 0 {
				t.Errorf("expected 0 errors, got %d for: %s", errs, tc.code)
			}
		})
	}
}

// TestMatchExpressions verifies match with various pattern types.
func TestMatchExpressions(t *testing.T) {
	cases := []struct {
		name string
		code string
	}{
		{"int_patterns", `main() { match x { 0 => a(), 1 => b(), _ => c(), } }`},
		{"string_patterns", `main() { match s { "a" => x(), "b" => y(), _ => z(), } }`},
		{"bool_patterns", `main() { match b { true => x(), false => y(), } }`},
		{"none_pattern", `main() { match opt { none => x(), _ => y(), } }`},
		{"name_pattern", `main() { match x { val => consume(val), } }`},
		{"wildcard", `main() { match x { _ => default(), } }`},
		{"enum_variant", `main() { match d { Dir.North => n(), Dir.South => s(), } }`},
		{"enum_destructure", `main() { match c { Color.Custom(r, g, b) => consume(r, g, b), _ => x(), } }`},
		{"short_destructure", `main() { match r { Ok(val) => consume(val), Err(e) => handle(e), } }`},
		{"type_binding", `main() { match s { Circle c => useCircle(c), _ => other(), } }`},
		{"guard", `main() { match n { x if x > 10 => big(), _ => small(), } }`},
		{"guard_complex", `main() { match n { x if x > 0 && x < 100 => mid(), _ => other(), } }`},
		{"block_arm", `main() { match x { 0 => { a(); b(); }, _ => c(), } }`},
		{"trailing_comma", `main() { match x { 0 => a(), 1 => b(), } }`},
		{"no_trailing_comma", `main() { match x { 0 => a(), 1 => b() } }`},
		{"as_statement", `main() { match x { _ => y(), } io.print_line("after"); }`},
		{"nested_match", `main() { match a { 0 => match b { 0 => x(), _ => y(), }, _ => z(), } }`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := parseString(tc.code)
			if errs != 0 {
				t.Errorf("expected 0 errors, got %d for: %s", errs, tc.code)
			}
		})
	}
}

// TestTypeDeclarations verifies type declaration syntax.
func TestTypeDeclarations(t *testing.T) {
	cases := []struct {
		name string
		code string
	}{
		{"empty_type", `type Foo {}`},
		{"fields_only", `type Point { Float x; Float y; }`},
		{"field_default", `type Config { Int port = 8080; }`},
		{"inheritance", `type Dog is Animal { String name; }`},
		{"multi_inherit", `type Foo is Bar, Baz {}`},
		{"method_abstract", `type Shape { area() Float ` + "`abstract; }"},
		{"method_instance", `type Foo { get(this) Int ` + "`instance { return 0; } }"},
		{"method_mut", `type Foo { set(~this, Int v) ` + "`instance { this.v = v; } }"},
		{"operator_method", `type Vec { +(Vec a, Vec b) Vec ` + "`static { return Vec(); } }"},
		{"generic_type", `type Box[T] { T value; }`},
		{"generic_constraint", `type Sortable[T: Comparable] { T[] items; }`},
		{"multi_constraint", `type Foo[T: A + B] {}`},
		{"meta_on_field", `type Foo { String name ` + "`doc(\"the name\"); }"},
		{"meta_on_type", `type Foo ` + "`deprecated {}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := parseString(tc.code)
			if errs != 0 {
				t.Errorf("expected 0 errors, got %d for: %s", errs, tc.code)
			}
		})
	}
}

// TestEnumDeclarations verifies enum declaration syntax.
func TestEnumDeclarations(t *testing.T) {
	cases := []struct {
		name string
		code string
	}{
		{"simple", `enum Dir { North, South, East, West }`},
		{"trailing_comma", `enum Dir { North, South, }`},
		{"with_fields", `enum Shape { Circle(Float r), Rect(Float w, Float h) }`},
		{"generic", `enum Option[T] { Some(T val), None }`},
		{"multi_generic", `enum Result[T, E] { Ok(T val), Err(E err) }`},
		{"single_variant", `enum Unit { Value }`},
		{"mixed", `enum Token { Eof, Number(Int val), Ident(String name) }`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := parseString(tc.code)
			if errs != 0 {
				t.Errorf("expected 0 errors, got %d for: %s", errs, tc.code)
			}
		})
	}
}

// TestErrorHandling verifies all error handling constructs.
func TestErrorHandling(t *testing.T) {
	cases := []struct {
		name string
		code string
	}{
		{"propagate", `f!() Int { Int x = g()?^; return x; }`},
		{"panic_bang", `f() { Int x = g()?!; }`},
		{"handler_named", `f!() Int { Int x = g() ? e { return 0; }; return x; }`},
		{"handler_unnamed", `f!() Int { Int x = g() ? { return 0; }; return x; }`},
		{"handler_discard", `f!() Int { Int x = g() ? _ { return 0; }; return x; }`},
		{"chained_propagate", `f!() Int { return a()?.b()?.c; }`},
		{"propagate_in_expr", `f!() Int { return g()?^ + 1; }`},
		{"raise", `f!() Int { raise Error("bad"); }`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := parseString(tc.code)
			if errs != 0 {
				t.Errorf("expected 0 errors, got %d for: %s", errs, tc.code)
			}
		})
	}
}

// TestCollections verifies array, map, and tuple syntax.
func TestCollections(t *testing.T) {
	cases := []struct {
		name string
		code string
	}{
		{"empty_array", `main() { Int[] a = []; }`},
		{"array_literal", `main() { Int[] a = [1, 2, 3]; }`},
		{"array_trailing_comma", `main() { Int[] a = [1, 2, 3,]; }`},
		{"array_single", `main() { Int[] a = [42]; }`},
		{"array_index", `main() { Int x = a[0]; }`},
		{"array_assign", `main() { a[0] = 42; }`},
		{"nested_array", `main() { Int[][] a = [[1, 2], [3, 4]]; }`},
		{"map_literal", `main() { m := {"a": 1, "b": 2}; }`},
		{"map_trailing_comma", `main() { m := {"a": 1, "b": 2,}; }`},
		{"map_single", `main() { m := {"key": "val"}; }`},
		{"map_expr_keys", `main() { m := {1 + 1: "two", 2 + 1: "three"}; }`},
		{"tuple_literal", `main() { t := (1, "hello"); }`},
		{"tuple_three", `main() { t := (1, 2, 3); }`},
		{"tuple_destructure", `main() { (a, b) := (10, 20); }`},
		{"slice_type", `main() { Int[] a = []; }`},
		{"array_type", `main() { Int[3] a = [1, 2, 3]; }`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := parseString(tc.code)
			if errs != 0 {
				t.Errorf("expected 0 errors, got %d for: %s", errs, tc.code)
			}
		})
	}
}

// TestConcurrency verifies go expressions and receive operator.
func TestConcurrency(t *testing.T) {
	cases := []struct {
		name string
		code string
	}{
		{"go_call", `main() { Task[Int] t = go compute(); }`},
		{"go_block", `main() { Task[Int] t = go { return 42; }; }`},
		{"receive", `main() { Int x = <-task; }`},
		{"receive_in_expr", `main() { Int x = <-task + 1; }`},
		{"go_and_receive", `main() { Task[Int] t = go work(); Int r = <-t; }`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := parseString(tc.code)
			if errs != 0 {
				t.Errorf("expected 0 errors, got %d for: %s", errs, tc.code)
			}
		})
	}
}

// TestOwnership verifies ownership and borrow syntax.
func TestOwnership(t *testing.T) {
	cases := []struct {
		name string
		code string
	}{
		{"shared_borrow_param", `f(String s) Int { return s.len(); }`},
		{"mut_borrow_param", `f(Int[] ~a) { a[0] = 1; }`},
		{"shared_ref_type", `main() { Shape &s = circle; }`},
		{"mut_ref_type", `main() { Int[] ~a = arr; }`},
		{"pointer_type", `main() { Int* p = addr; }`},
		{"optional_ref", `main() { Shape&? s = none; }`},
		{"unsafe_block", `main() { unsafe { Int x = 42; } }`},
		{"unsafe_as_expr", `main() { Int x = unsafe { return 42; }; }`},
		{"move_lambda", `main() { f := move |x| -> x; }`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := parseString(tc.code)
			if errs != 0 {
				t.Errorf("expected 0 errors, got %d for: %s", errs, tc.code)
			}
		})
	}
}

// TestMetaAnnotations verifies meta annotation syntax.
func TestMetaAnnotations(t *testing.T) {
	cases := []struct {
		name string
		code string
	}{
		{"simple", "type Foo `deprecated {}"},
		{"with_value", "type Foo { String name `default(\"hi\"); }"},
		{"with_named_param", "type Foo { Int x `range(min: 0, max: 100); }"},
		{"multiple", "type Foo { Bool x `doc(\"help\") `deprecated; }"},
		{"on_method", "type Foo { f() `instance `inline; }"},
		{"on_enum", "enum E `serializable { A, B }"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := parseString(tc.code)
			if errs != 0 {
				t.Errorf("expected 0 errors, got %d for: %s", errs, tc.code)
			}
		})
	}
}

// TestEdgeCases verifies unusual but valid syntax.
func TestEdgeCases(t *testing.T) {
	cases := []struct {
		name string
		code string
	}{
		{"empty_file", ``},
		{"only_use", `use io "std/io";`},
		{"multiple_use", `use io "std/io"; use fs "std/fs";`},
		{"empty_function", `f() {}`},
		{"empty_params", `f() Int { return 0; }`},
		{"empty_block", `main() { for { break; } }`},
		{"deeply_nested_expr", `main() { Int x = ((((1 + 2)))); }`},
		{"long_chain", `main() { a.b.c.d.e.f.g(); }`},
		{"long_optional_chain", `main() { a?.b?.c?.d; }`},
		{"multiple_assignments", `main() { Int x = 0; x = 1; x += 2; x -= 1; x *= 3; x /= 2; x %= 5; }`},
		{"increment", `main() { Int x = 0; x++; }`},
		{"decrement", `main() { Int x = 5; x--; }`},
		{"inc_dec_for_loop", `main() { for i := 0; i < 10; i++ { print_line(i); } }`},
		{"dec_for_loop", `main() { for i := 10; i > 0; i-- { print_line(i); } }`},
		{"member_increment", `type C { Int n; } main() { C c = C(n: 0); c.n++; }`},
		{"yield_in_loop", `gen() Stream[Int] { for x in 0..10 { yield x; } }`},
		{"yield_delegate", `gen() Stream[Int] { yield* other(); }`},
		{"complex_type_ref", `main() { Int[][]? x = none; }`},
		{"function_type", `main() { (Int, Int) -> Int fn = add; }`},
		{"tuple_type", `main() { (Int, String) t = (1, "a"); }`},
		{"nested_generics", `main() { Map[String, List[Int]] m = {"a": [1]}; }`},
		{"comments_everywhere", `
// top comment
use io "std/io"; // use comment
/* block comment */
main() { /* inside */ Int x = 0; // end
}`},
		{"if_as_expr", `main() { Int x = if a > 0 { 1; } else { 0; }; }`},
		{"match_as_expr", `main() { Int x = match n { 0 => 1, _ => 2, }; }`},
		{"named_args", `main() { f(x: 1, y: 2); }`},
		{"mixed_args", `main() { f(1, y: 2); }`},
		{"return_no_value", `f() { return; }`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := parseString(tc.code)
			if errs != 0 {
				t.Errorf("expected 0 errors, got %d for: %s", errs, tc.code)
			}
		})
	}
}

// TestParseSourceFileNormalizesCRLF verifies that parseSourceFile normalizes
// CRLF line endings to LF before lexing, so that triple-quoted and raw string
// literals have consistent content regardless of how git checked the file out
// (CRLF on Windows vs LF elsewhere). Regression test for B0339.
func TestParseSourceFileNormalizesCRLF(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "crlf.pr")
	// Source with CRLF line endings containing a triple-quoted multiline string.
	source := "f() {\r\n  s := \"\"\"\r\nline one\r\nline two\r\n\"\"\";\r\n}\r\n"
	if err := os.WriteFile(path, []byte(source), 0644); err != nil {
		t.Fatal(err)
	}
	file := parseSourceFile(path)
	if len(file.Decls) != 1 {
		t.Fatalf("expected 1 decl, got %d", len(file.Decls))
	}
	fn, ok := file.Decls[0].(*ast.FuncDecl)
	if !ok {
		t.Fatalf("expected FuncDecl, got %T", file.Decls[0])
	}
	if fn.Body == nil || len(fn.Body.Stmts) != 1 {
		t.Fatalf("expected 1 stmt in func body")
	}
	decl, ok := fn.Body.Stmts[0].(*ast.InferredVarDecl)
	if !ok {
		t.Fatalf("expected InferredVarDecl, got %T", fn.Body.Stmts[0])
	}
	sl, ok := decl.Value.(*ast.StringLit)
	if !ok {
		t.Fatalf("expected StringLit, got %T", decl.Value)
	}
	if strings.Contains(sl.Raw, "\r") {
		t.Errorf("StringLit.Raw contains \\r after parse: %q", sl.Raw)
	}
	want := "\"\"\"\nline one\nline two\n\"\"\""
	if sl.Raw != want {
		t.Errorf("StringLit.Raw = %q, want %q", sl.Raw, want)
	}
}

// TestTryParseSourceString verifies that tryParseSourceString correctly identifies
// complete Promise files (with a main function) vs expressions that need wrapping.
func TestTryParseSourceString(t *testing.T) {
	hasMain := []struct {
		name string
		code string
	}{
		{"main_function", `main() { print_line("hi"); }`},
		{"main_failable", `main!() { print_line("hi"); }`},
		{"main_failable_spaced", "main ! () { print_line(\"hi\"); }"},
		{"main_with_helper", `add(int a, int b) int { return a + b; } main() { print_line("x"); }`},
		{"type_and_main", `type Foo { int x; } main() { f := Foo(x: 1); }`},
		{"enum_and_main", `enum Dir { N, S } main() { d := Dir.N; }`},
	}
	for _, tc := range hasMain {
		t.Run("hasMain/"+tc.name, func(t *testing.T) {
			_, ok := tryParseSourceString(tc.code)
			if !ok {
				t.Errorf("expected tryParseSourceString to succeed for: %s", tc.code)
			}
		})
	}

	noMain := []struct {
		name string
		code string
	}{
		{"bare_call", `print_line(42)`},
		{"bare_print_line", `print_line("hello")`},
		{"string_with_main", `print_line("main")`},
		{"assignment", `x := 10; print_line(x)`},
		{"multi_statement", `x := 10; y := 20; print_line(x + y)`},
		{"if_statement", `if true { print_line(1); }`},
		{"for_loop", `for i in 0..10 { print_line(i); }`},
		{"type_only", `type Point { int x; int y; }`},
		{"enum_only", `enum Color { Red, Green, Blue }`},
		{"helper_only", `add(int a, int b) int { return a + b; }`},
	}
	for _, tc := range noMain {
		t.Run("noMain/"+tc.name, func(t *testing.T) {
			_, ok := tryParseSourceString(tc.code)
			if ok {
				t.Errorf("expected tryParseSourceString to fail (no main) for: %s", tc.code)
			}
		})
	}
}

// TestExecWrapCode verifies that expression wrapping produces parseable code.
// Uses the same wrapping logic as runExec: add ";" only if source doesn't
// already end with ";" or "}".
func TestExecWrapCode(t *testing.T) {
	cases := []struct {
		name   string
		source string
	}{
		{"bare_call", `print_line(42)`},
		{"with_semi", `print_line(42);`},
		{"multi_statement", `x := 10; print_line(x);`},
		{"string_call", `print_line("hello")`},
		{"if_stmt", `if true { print_line(1); }`},
		{"for_loop", `for i := 0; i < 3; i += 1 { print_line(i); }`},
		{"for_loop_inc", `for i := 0; i < 3; i++ { print_line(i); }`},
		{"increment", `x := 0; x++;`},
		{"while_loop", `while true { break; }`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := tc.source
			if !strings.HasSuffix(source, ";") && !strings.HasSuffix(source, "}") {
				source += ";"
			}
			wrapped := "main!() {\n" + source + "\n}"
			errs := parseString(wrapped)
			if errs != 0 {
				t.Errorf("wrapped code has parse errors: %s", wrapped)
			}
		})
	}
}

func TestExtractUseDecls(t *testing.T) {
	cases := []struct {
		name     string
		source   string
		wantUses string
		wantBody string
	}{
		{"no_uses", `print_line(42)`, "", `print_line(42)`},
		{"single_use", `use math; print_line(42)`, "use math;", `print_line(42)`},
		{"use_with_alias", `use math as m; print_line(42)`, "use math as m;", `print_line(42)`},
		{"multiple_uses", "use math;\nuse json;\nprint_line(42)", "use math;\nuse json;", `print_line(42)`},
		{"use_var_binding", `use x := 5; print_line(x)`, "", `use x := 5; print_line(x)`},
		{"use_no_semicolon", `use math`, "", `use math`},
		{"use_only", `use math;`, "use math;", ""},
		{"use_with_leading_whitespace", "  use math; code", "use math;", "code"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotUses, gotBody := extractUseDecls(tc.source)
			if gotUses != tc.wantUses {
				t.Errorf("uses: got %q, want %q", gotUses, tc.wantUses)
			}
			if gotBody != tc.wantBody {
				t.Errorf("body: got %q, want %q", gotBody, tc.wantBody)
			}
		})
	}
}

// TestAutoInjectCatalogUses verifies that references to catalog modules
// (via `name.X` member access) trigger automatic injection of `use <name>;`
// declarations, and that existing explicit `use` declarations are respected.
// T0221.
func TestAutoInjectCatalogUses(t *testing.T) {
	cases := []struct {
		name   string
		source string
		want   string // prefix that must appear at start; empty = no injection
	}{
		{"no_refs", `print_line(42)`, ""},
		{"bare_ident_no_dot", `print_line(io)`, ""},
		{"member_access", `print_line(io.File.read_content("x"))`, "use io;\n"},
		{"already_explicit", `use io; print_line(io.File.read_content("x"))`, ""},
		{"alias_covers", `use io as x; print_line(x.File.read_content("x"))`, ""},
		{"multiple_modules", `os.execute("ls"); io.File.read_content("x")`, "use io;\nuse os;\n"},
		{"mixed_explicit_and_injected", `use os; io.File.read_content("x"); os.execute("ls")`, "use io;\n"},
		{"nested_member_skipped", `x.io.foo`, ""},
		{"std_ignored", `std.x`, ""},
		{"string_interp_undetected", `"hello {io.x}"`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := autoInjectCatalogUses(tc.source)
			if tc.want == "" {
				if got != tc.source {
					t.Errorf("expected no injection, got %q", got)
				}
				return
			}
			// Exact match: injection is a pure prefix, no other mutation.
			// This also catches duplicate-injection regressions.
			if got != tc.want+tc.source {
				t.Errorf("got %q, want %q", got, tc.want+tc.source)
			}
		})
	}
}

func TestExecWrapCodeWithUseDecls(t *testing.T) {
	cases := []struct {
		name   string
		source string
	}{
		{"use_and_call", `use math; print_line(math.lerp(0.0, 10.0, 0.5).to_string());`},
		{"use_alias_and_call", `use math as m; print_line(m.lerp(0.0, 10.0, 0.5).to_string());`},
		{"multi_use", "use math;\nuse json;\nprint_line(math.lerp(0.0, 10.0, 0.5).to_string());"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usePrefix, body := extractUseDecls(tc.source)
			if !strings.HasSuffix(body, ";") && !strings.HasSuffix(body, "}") {
				body += ";"
			}
			wrapped := "main!() {\n" + body + "\n}"
			if usePrefix != "" {
				wrapped = usePrefix + "\n" + wrapped
			}
			errs := parseString(wrapped)
			if errs != 0 {
				t.Errorf("wrapped code has parse errors: %s", wrapped)
			}
		})
	}
}

// --- Module loading integration tests ---

// testModuleLoader creates a moduleLoader for use in tests.
func testModuleLoader(projectDir string) *moduleLoader {
	return testModuleLoaderWithConfig(projectDir, nil)
}

func testModuleLoaderWithConfig(projectDir string, cfg *module.Config) *moduleLoader {
	commitPins := make(map[string]string)
	namedRequire := make(map[string]*module.RequireEntry)
	if cfg != nil {
		for url, pin := range cfg.Require {
			commitPins[module.NormalizeURL(url)] = pin
		}
		for name, entry := range cfg.NamedRequire {
			commitPins[module.NormalizeURL(entry.URL)] = entry.Commit
			namedRequire[name] = entry
		}
	}
	var cat *module.Catalog
	if len(embeddedCatalog) > 0 {
		if c, err := module.ParseCatalog(embeddedCatalog); err == nil {
			cat = c
		}
	}
	return &moduleLoader{
		projectRoot:      projectDir,
		projectCfg:       cfg,
		namedRequire:     namedRequire,
		loaded:           make(map[string]*sema.ModuleInfo),
		globalIdentities: make(map[string]string),
		visiting:         make(map[string]string),
		allModInfos:      make(map[string]*sema.ModuleInfo),
		remoteResolved:   make(map[string]string),
		catalogLoaded:    make(map[string]*sema.ModuleInfo),
		commitPins:       commitPins,
		catalog:          cat,
		target:           sema.HostTargetInfo(),
	}
}

// TestLoadLocalModuleBasic creates a temp module directory and verifies
// that loadLocalModule parses, sema-checks, and extracts the exported scope.
func TestLoadLocalModuleBasic(t *testing.T) {
	// Create project structure:
	//   project/
	//     promise.toml
	//     libs/mymod/
	//       promise.toml
	//       lib.pr
	projectDir := t.TempDir()
	modDir := filepath.Join(projectDir, "libs", "mymod")
	if err := os.MkdirAll(modDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write project promise.toml
	if err := os.WriteFile(filepath.Join(projectDir, "promise.toml"), []byte(`
[module]
name = "testproj"
epoch = "2026.0"
`), 0644); err != nil {
		t.Fatal(err)
	}

	// Write module promise.toml
	if err := os.WriteFile(filepath.Join(modDir, "promise.toml"), []byte(`
[module]
name = "mymod"
epoch = "2026.0"
`), 0644); err != nil {
		t.Fatal(err)
	}

	// Write module source
	if err := os.WriteFile(filepath.Join(modDir, "lib.pr"), []byte(`
type User `+"`public"+` { int id; }
create_user() int `+"`public"+` { return 0; }
helper() int { return 1; }
`), 0644); err != nil {
		t.Fatal(err)
	}

	// Load the module (with std so sema validation passes)
	loader := testModuleLoader(projectDir)
	modInfo, err := loader.load("./libs/mymod")
	if err != nil {
		t.Fatalf("loader.load failed: %v", err)
	}
	if modInfo == nil {
		t.Fatal("expected non-nil ModuleInfo")
	}

	// Verify only public symbols are in the exported scope
	scope := sema.ExportedScope(modInfo.SemaInfo, modInfo.File)
	if scope.Lookup("User") == nil {
		t.Error("expected 'User' in exported scope")
	}
	if scope.Lookup("create_user") == nil {
		t.Error("expected 'create_user' in exported scope")
	}
	if scope.Lookup("helper") != nil {
		t.Error("'helper' should not be in exported scope (not public)")
	}
}

// TestLoadLocalModuleMultipleFiles verifies that multiple .pr files in a module
// directory are all parsed and merged.
func TestLoadLocalModuleMultipleFiles(t *testing.T) {
	projectDir := t.TempDir()
	modDir := filepath.Join(projectDir, "mylib")
	if err := os.MkdirAll(modDir, 0755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(projectDir, "promise.toml"), []byte(`
[module]
name = "testproj"
epoch = "2026.0"
`), 0644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(modDir, "promise.toml"), []byte(`
[module]
name = "mylib"
epoch = "2026.0"
`), 0644); err != nil {
		t.Fatal(err)
	}

	// Two files in the module, each exporting different things
	if err := os.WriteFile(filepath.Join(modDir, "a.pr"), []byte(`
type Foo `+"`public"+` { int x; }
`), 0644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(modDir, "b.pr"), []byte(`
type Bar `+"`public"+` { int y; }
`), 0644); err != nil {
		t.Fatal(err)
	}

	loader := testModuleLoader(projectDir)
	modInfo, err := loader.load("./mylib")
	if err != nil {
		t.Fatalf("loader.load failed: %v", err)
	}

	scope := sema.ExportedScope(modInfo.SemaInfo, modInfo.File)
	if scope.Lookup("Foo") == nil {
		t.Error("expected 'Foo' from a.pr in exported scope")
	}
	if scope.Lookup("Bar") == nil {
		t.Error("expected 'Bar' from b.pr in exported scope")
	}
}

// TestLoadLocalModuleNoPromiseToml verifies error when module dir has no promise.toml.
func TestLoadLocalModuleNoPromiseToml(t *testing.T) {
	projectDir := t.TempDir()
	modDir := filepath.Join(projectDir, "badmod")
	if err := os.MkdirAll(modDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modDir, "lib.pr"), []byte("helper() int { return 0; }"), 0644); err != nil {
		t.Fatal(err)
	}

	loader := testModuleLoader(projectDir)
	_, err := loader.load("./badmod")
	if err == nil {
		t.Fatal("expected error for missing promise.toml")
	}
	if !strings.Contains(err.Error(), "promise.toml") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestLoadLocalModuleDirNotFound verifies error when module directory doesn't exist.
func TestLoadLocalModuleDirNotFound(t *testing.T) {
	projectDir := t.TempDir()
	loader := testModuleLoader(projectDir)
	_, err := loader.load("./nonexistent")
	if err == nil {
		t.Fatal("expected error for missing directory")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestLoadLocalModuleNoPrFiles verifies error when module has no .pr files.
func TestLoadLocalModuleNoPrFiles(t *testing.T) {
	projectDir := t.TempDir()
	modDir := filepath.Join(projectDir, "empty")
	if err := os.MkdirAll(modDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modDir, "promise.toml"), []byte(`
[module]
name = "empty"
epoch = "2026.0"
`), 0644); err != nil {
		t.Fatal(err)
	}

	loader := testModuleLoader(projectDir)
	_, err := loader.load("./empty")
	if err == nil {
		t.Fatal("expected error for module with no .pr files")
	}
	if !strings.Contains(err.Error(), "no .pr files") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestLoadLocalModuleSemaErrors verifies that sema errors in a module are reported.
func TestLoadLocalModuleSemaErrors(t *testing.T) {
	projectDir := t.TempDir()
	modDir := filepath.Join(projectDir, "badmod")
	if err := os.MkdirAll(modDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modDir, "promise.toml"), []byte(`
[module]
name = "badmod"
epoch = "2026.0"
`), 0644); err != nil {
		t.Fatal(err)
	}
	// Module source with a type error: returning string where int expected
	if err := os.WriteFile(filepath.Join(modDir, "lib.pr"), []byte(`
compute() int `+"`public"+` { return "not an int"; }
`), 0644); err != nil {
		t.Fatal(err)
	}

	loader := testModuleLoader(projectDir)
	_, err := loader.load("./badmod")
	if err == nil {
		t.Fatal("expected error for module with sema errors")
	}
	if !strings.Contains(err.Error(), "errors in module") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestLoadLocalModuleWithStdTypes verifies a module using std types (string, int[]) loads correctly.
func TestLoadLocalModuleWithStdTypes(t *testing.T) {
	projectDir := t.TempDir()
	modDir := filepath.Join(projectDir, "mymod")
	if err := os.MkdirAll(modDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "promise.toml"), []byte(`
[module]
name = "testproj"
epoch = "2026.0"
`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modDir, "promise.toml"), []byte(`
[module]
name = "mymod"
epoch = "2026.0"
`), 0644); err != nil {
		t.Fatal(err)
	}
	// Module uses std library types: string return, int[] parameter
	if err := os.WriteFile(filepath.Join(modDir, "lib.pr"), []byte(`
greet(string name) string `+"`public"+` { return "hello " + name; }
sum(int[] nums) int `+"`public"+` {
	int total = 0;
	for n in nums { total = total + n; }
	return total;
}
`), 0644); err != nil {
		t.Fatal(err)
	}

	loader := testModuleLoader(projectDir)
	modInfo, err := loader.load("./mymod")
	if err != nil {
		t.Fatalf("loader.load failed: %v", err)
	}
	scope := sema.ExportedScope(modInfo.SemaInfo, modInfo.File)
	if scope.Lookup("greet") == nil {
		t.Error("expected 'greet' in exported scope")
	}
	if scope.Lookup("sum") == nil {
		t.Error("expected 'sum' in exported scope")
	}
}

// TestLoadModuleTransitive verifies that modules can import other modules.
// Module B depends on module A; loading B should recursively load A first.
func TestLoadModuleTransitive(t *testing.T) {
	projectDir := t.TempDir()
	modA := filepath.Join(projectDir, "moda")
	modB := filepath.Join(projectDir, "modb")
	if err := os.MkdirAll(modA, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(modB, 0755); err != nil {
		t.Fatal(err)
	}

	for _, item := range []struct{ path, content string }{
		{filepath.Join(projectDir, "promise.toml"), "[module]\nname = \"proj\"\nepoch = \"2026.0\"\n"},
		{filepath.Join(modA, "promise.toml"), "[module]\nname = \"moda\"\nepoch = \"2026.0\"\n"},
		{filepath.Join(modA, "lib.pr"), "helper() int `public { return 42; }\n"},
		{filepath.Join(modB, "promise.toml"), "[module]\nname = \"modb\"\nepoch = \"2026.0\"\n"},
		{filepath.Join(modB, "lib.pr"), "use moda \"./moda\";\nwrap() int `public { return moda.helper(); }\n"},
	} {
		if err := os.WriteFile(item.path, []byte(item.content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	loader := testModuleLoader(projectDir)
	modInfo, err := loader.load("./modb")
	if err != nil {
		t.Fatalf("loader.load failed: %v", err)
	}
	if modInfo == nil {
		t.Fatal("expected non-nil ModuleInfo")
	}
	if modInfo.CanonicalName != "modb" {
		t.Errorf("expected canonical name 'modb', got '%s'", modInfo.CanonicalName)
	}

	// moda should also be in allModInfos (transitive dependency)
	if _, ok := loader.allModInfos["./moda"]; !ok {
		t.Error("expected moda in allModInfos (transitive dep)")
	}
	if _, ok := loader.allModInfos["./modb"]; !ok {
		t.Error("expected modb in allModInfos")
	}

	// depOrder should contain [moda, modb] in order — deps before dependents.
	// Catalog modules (e.g. std) may also appear in depOrder; filter to local paths only.
	var localOrder []string
	for _, p := range loader.depOrder {
		if strings.HasPrefix(p, "./") {
			localOrder = append(localOrder, p)
		}
	}
	if len(localOrder) != 2 {
		t.Fatalf("expected 2 local entries in depOrder, got %d: %v", len(localOrder), loader.depOrder)
	}
	if localOrder[0] != "./moda" || localOrder[1] != "./modb" {
		t.Errorf("expected depOrder [./moda, ./modb], got %v", localOrder)
	}
}

// TestLoadModuleDiamond verifies that diamond dependencies are handled correctly.
// Both B and C depend on A; loading them should not produce errors or duplicates.
func TestLoadModuleDiamond(t *testing.T) {
	projectDir := t.TempDir()
	modA := filepath.Join(projectDir, "a")
	modB := filepath.Join(projectDir, "b")
	modC := filepath.Join(projectDir, "c")
	for _, d := range []string{modA, modB, modC} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}

	for _, item := range []struct{ path, content string }{
		{filepath.Join(projectDir, "promise.toml"), "[module]\nname = \"proj\"\nepoch = \"2026.0\"\n"},
		{filepath.Join(modA, "promise.toml"), "[module]\nname = \"a\"\nepoch = \"2026.0\"\n"},
		{filepath.Join(modA, "lib.pr"), "base() int `public { return 1; }\n"},
		{filepath.Join(modB, "promise.toml"), "[module]\nname = \"b\"\nepoch = \"2026.0\"\n"},
		{filepath.Join(modB, "lib.pr"), "use a \"./a\";\nfrom_b() int `public { return a.base(); }\n"},
		{filepath.Join(modC, "promise.toml"), "[module]\nname = \"c\"\nepoch = \"2026.0\"\n"},
		{filepath.Join(modC, "lib.pr"), "use a \"./a\";\nfrom_c() int `public { return a.base(); }\n"},
	} {
		if err := os.WriteFile(item.path, []byte(item.content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	loader := testModuleLoader(projectDir)

	// Load both B and C
	_, err := loader.load("./b")
	if err != nil {
		t.Fatalf("loader.load(b) failed: %v", err)
	}
	_, err = loader.load("./c")
	if err != nil {
		t.Fatalf("loader.load(c) failed: %v", err)
	}

	// A, B, C should all be in allModInfos (catalog modules may also appear)
	if _, ok := loader.allModInfos["./a"]; !ok {
		t.Error("expected ./a in allModInfos")
	}
	if _, ok := loader.allModInfos["./b"]; !ok {
		t.Error("expected ./b in allModInfos")
	}
	if _, ok := loader.allModInfos["./c"]; !ok {
		t.Error("expected ./c in allModInfos")
	}

	// depOrder should have ./a before ./b and ./c — filter to local paths only.
	var localOrder []string
	for _, p := range loader.depOrder {
		if strings.HasPrefix(p, "./") {
			localOrder = append(localOrder, p)
		}
	}
	if len(localOrder) != 3 {
		t.Fatalf("expected 3 local entries in depOrder, got %d: %v", len(localOrder), loader.depOrder)
	}
	if localOrder[0] != "./a" {
		t.Errorf("expected ./a first in local depOrder, got %s", localOrder[0])
	}
}

// TestLoadModuleCircular verifies that circular dependencies are detected.
func TestLoadModuleCircular(t *testing.T) {
	projectDir := t.TempDir()
	modA := filepath.Join(projectDir, "x")
	modB := filepath.Join(projectDir, "y")
	for _, d := range []string{modA, modB} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}

	for _, item := range []struct{ path, content string }{
		{filepath.Join(projectDir, "promise.toml"), "[module]\nname = \"proj\"\nepoch = \"2026.0\"\n"},
		{filepath.Join(modA, "promise.toml"), "[module]\nname = \"x\"\nepoch = \"2026.0\"\n"},
		{filepath.Join(modA, "lib.pr"), "use y \"./y\";\nfx() int `public { return 1; }\n"},
		{filepath.Join(modB, "promise.toml"), "[module]\nname = \"y\"\nepoch = \"2026.0\"\n"},
		{filepath.Join(modB, "lib.pr"), "use x \"./x\";\nfy() int `public { return 1; }\n"},
	} {
		if err := os.WriteFile(item.path, []byte(item.content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	loader := testModuleLoader(projectDir)
	_, err := loader.load("./x")
	if err == nil {
		t.Fatal("expected error for circular dependency")
	}
	if !strings.Contains(err.Error(), "circular dependency") {
		t.Errorf("expected 'circular dependency' error, got: %v", err)
	}
}

// TestLoadModuleCircularThreeModules verifies cycle detection through 3 modules: A→B→C→A.
func TestLoadModuleCircularThreeModules(t *testing.T) {
	projectDir := t.TempDir()
	for _, d := range []string{"a", "b", "c"} {
		if err := os.MkdirAll(filepath.Join(projectDir, d), 0755); err != nil {
			t.Fatal(err)
		}
	}

	for _, item := range []struct{ path, content string }{
		{filepath.Join(projectDir, "promise.toml"), "[module]\nname = \"proj\"\nepoch = \"2026.0\"\n"},
		{filepath.Join(projectDir, "a", "promise.toml"), "[module]\nname = \"a\"\nepoch = \"2026.0\"\n"},
		{filepath.Join(projectDir, "a", "lib.pr"), "use b \"./b\";\nfa() int `public { return 1; }\n"},
		{filepath.Join(projectDir, "b", "promise.toml"), "[module]\nname = \"b\"\nepoch = \"2026.0\"\n"},
		{filepath.Join(projectDir, "b", "lib.pr"), "use c \"./c\";\nfb() int `public { return 2; }\n"},
		{filepath.Join(projectDir, "c", "promise.toml"), "[module]\nname = \"c\"\nepoch = \"2026.0\"\n"},
		{filepath.Join(projectDir, "c", "lib.pr"), "use a \"./a\";\nfc() int `public { return 3; }\n"},
	} {
		if err := os.WriteFile(item.path, []byte(item.content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	loader := testModuleLoader(projectDir)
	_, err := loader.load("./a")
	if err == nil {
		t.Fatal("expected error for 3-module circular dependency")
	}
	if !strings.Contains(err.Error(), "circular dependency") {
		t.Errorf("expected 'circular dependency' error, got: %v", err)
	}
	// The cycle path should mention all 3 modules
	errMsg := err.Error()
	if !strings.Contains(errMsg, "./a") || !strings.Contains(errMsg, "./b") || !strings.Contains(errMsg, "./c") {
		t.Errorf("expected cycle path to mention a, b, c; got: %v", errMsg)
	}
}

// TestLoadModuleCanonicalName verifies that the canonical name comes from promise.toml,
// and GlobalIdentity is derived from the import path.
func TestLoadModuleCanonicalName(t *testing.T) {
	projectDir := t.TempDir()
	modDir := filepath.Join(projectDir, "my-local-path")
	if err := os.MkdirAll(modDir, 0755); err != nil {
		t.Fatal(err)
	}

	for _, item := range []struct{ path, content string }{
		{filepath.Join(projectDir, "promise.toml"), "[module]\nname = \"proj\"\nepoch = \"2026.0\"\n"},
		{filepath.Join(modDir, "promise.toml"), "[module]\nname = \"my_canonical\"\nepoch = \"2026.0\"\n"},
		{filepath.Join(modDir, "lib.pr"), "greet() int `public { return 1; }\n"},
	} {
		if err := os.WriteFile(item.path, []byte(item.content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	loader := testModuleLoader(projectDir)
	modInfo, err := loader.load("./my-local-path")
	if err != nil {
		t.Fatalf("loader.load failed: %v", err)
	}
	if modInfo.CanonicalName != "my_canonical" {
		t.Errorf("expected canonical name 'my_canonical', got '%s'", modInfo.CanonicalName)
	}
	// GlobalIdentity should be the import path, not the canonical name
	if modInfo.GlobalIdentity != "./my-local-path" {
		t.Errorf("expected GlobalIdentity './my-local-path', got '%s'", modInfo.GlobalIdentity)
	}
	// IRPrefix should be derived from GlobalIdentity (sanitized path)
	expectedPrefix := module.SanitizeIRPrefix("./my-local-path")
	if modInfo.IRPrefix != expectedPrefix {
		t.Errorf("expected IRPrefix '%s', got '%s'", expectedPrefix, modInfo.IRPrefix)
	}
}

// TestLoadModuleSameNameDifferentPaths verifies that two modules with the same
// promise.toml name but different import paths can coexist (no collision).
// This is the key improvement of GlobalIdentity over the old CanonicalName system.
func TestLoadModuleSameNameDifferentPaths(t *testing.T) {
	projectDir := t.TempDir()
	modA := filepath.Join(projectDir, "mod_a")
	modB := filepath.Join(projectDir, "mod_b")
	for _, d := range []string{modA, modB} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}

	for _, item := range []struct{ path, content string }{
		{filepath.Join(projectDir, "promise.toml"), "[module]\nname = \"proj\"\nepoch = \"2026.0\"\n"},
		// Both modules claim the same name "parser" — but they have different paths
		{filepath.Join(modA, "promise.toml"), "[module]\nname = \"parser\"\nepoch = \"2026.0\"\n"},
		{filepath.Join(modA, "lib.pr"), "fa() int `public { return 1; }\n"},
		{filepath.Join(modB, "promise.toml"), "[module]\nname = \"parser\"\nepoch = \"2026.0\"\n"},
		{filepath.Join(modB, "lib.pr"), "fb() int `public { return 2; }\n"},
	} {
		if err := os.WriteFile(item.path, []byte(item.content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	loader := testModuleLoader(projectDir)

	// Load first module — should succeed
	miA, err := loader.load("./mod_a")
	if err != nil {
		t.Fatalf("loader.load(mod_a) failed: %v", err)
	}

	// Load second module with same name but different path — should also succeed
	miB, err := loader.load("./mod_b")
	if err != nil {
		t.Fatalf("loader.load(mod_b) failed: %v", err)
	}

	// Both have same CanonicalName but different GlobalIdentity and IRPrefix
	if miA.CanonicalName != "parser" || miB.CanonicalName != "parser" {
		t.Errorf("expected both to have canonical name 'parser'")
	}
	if miA.GlobalIdentity == miB.GlobalIdentity {
		t.Errorf("expected different global identities, both got %q", miA.GlobalIdentity)
	}
	if miA.IRPrefix == miB.IRPrefix {
		t.Errorf("expected different IR prefixes, both got %q", miA.IRPrefix)
	}
}

// TestLoadRemoteModuleReplace verifies that [replace] redirects a remote URL to a local path.
func TestLoadRemoteModuleReplace(t *testing.T) {
	projectDir := t.TempDir()

	// Create the local replacement module
	modDir := filepath.Join(projectDir, "local-parser")
	os.MkdirAll(modDir, 0755)
	os.WriteFile(filepath.Join(modDir, "promise.toml"), []byte(`
[module]
name = "parser"
epoch = "2026.0"
`), 0644)
	os.WriteFile(filepath.Join(modDir, "parser.pr"), []byte(`
parse(int x) int `+"`"+`public {
    return x + 1;
}
`), 0644)

	// Root project config with [replace]
	cfg := &module.Config{
		Name:    "myproject",
		Epoch:   "2026.0",
		Dir:     projectDir,
		Require: map[string]string{},
		Replace: map[string]string{
			"github.com/someone/parser": "./local-parser",
		},
	}

	loader := testModuleLoaderWithConfig(projectDir, cfg)

	modInfo, err := loader.loadRemote("github.com/someone/parser", "parser")
	if err != nil {
		t.Fatalf("loadRemote with [replace] failed: %v", err)
	}
	if modInfo == nil {
		t.Fatal("expected non-nil ModuleInfo")
	}
	if modInfo.CanonicalName != "parser" {
		t.Errorf("expected canonical name 'parser', got %q", modInfo.CanonicalName)
	}
	// GlobalIdentity should be the remote URL (not the local path)
	if modInfo.GlobalIdentity != "github.com/someone/parser" {
		t.Errorf("expected GlobalIdentity 'github.com/someone/parser', got %q", modInfo.GlobalIdentity)
	}

	// Verify it's cached in remoteResolved
	normalized := module.NormalizeURL("github.com/someone/parser")
	if _, ok := loader.remoteResolved[normalized]; !ok {
		t.Error("expected URL to be cached in remoteResolved")
	}

	// Second call should return the same module (dedup)
	modInfo2, err := loader.loadRemote("github.com/someone/parser", "parser")
	if err != nil {
		t.Fatalf("second loadRemote failed: %v", err)
	}
	if modInfo2.AbsDir != modInfo.AbsDir {
		t.Error("expected same module on second load")
	}
}

// TestLoadRemoteModuleReplaceSchemeVariant verifies that [replace] matches
// URL variants (with/without scheme).
func TestLoadRemoteModuleReplaceSchemeVariant(t *testing.T) {
	projectDir := t.TempDir()

	modDir := filepath.Join(projectDir, "local-parser")
	os.MkdirAll(modDir, 0755)
	os.WriteFile(filepath.Join(modDir, "promise.toml"), []byte(`
[module]
name = "parser"
epoch = "2026.0"
`), 0644)
	os.WriteFile(filepath.Join(modDir, "parser.pr"), []byte(`
parse(int x) int `+"`"+`public { return x; }
`), 0644)

	// Replace key uses bare URL, import uses https://
	cfg := &module.Config{
		Name:    "myproject",
		Epoch:   "2026.0",
		Dir:     projectDir,
		Require: map[string]string{},
		Replace: map[string]string{
			"github.com/someone/parser": "./local-parser",
		},
	}

	loader := testModuleLoaderWithConfig(projectDir, cfg)

	// Import with https:// scheme — should still match the replace
	modInfo, err := loader.loadRemote("https://github.com/someone/parser", "parser")
	if err != nil {
		t.Fatalf("loadRemote with scheme variant failed: %v", err)
	}
	if modInfo.CanonicalName != "parser" {
		t.Errorf("expected canonical name 'parser', got %q", modInfo.CanonicalName)
	}
}

// TestLoadRemoteModuleNoPinError verifies that a remote module without a pin produces a clear error.
func TestLoadRemoteModuleNoPinError(t *testing.T) {
	projectDir := t.TempDir()

	cfg := &module.Config{
		Name:    "myproject",
		Epoch:   "2026.0",
		Dir:     projectDir,
		Require: map[string]string{}, // no pins
		Replace: map[string]string{},
	}

	loader := testModuleLoaderWithConfig(projectDir, cfg)

	_, err := loader.loadRemote("github.com/someone/parser", "parser")
	if err == nil {
		t.Fatal("expected error for missing pin")
	}
	if !strings.Contains(err.Error(), "no pin") {
		t.Errorf("expected 'no pin' error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "promise.toml") {
		t.Errorf("expected error to mention promise.toml, got: %v", err)
	}
}

// TestLoadRemoteModuleNilConfig verifies loadRemote works when there's no promise.toml (single-file mode).
func TestLoadRemoteModuleNilConfig(t *testing.T) {
	projectDir := t.TempDir()
	// nil config — no [require], no [replace]
	loader := testModuleLoaderWithConfig(projectDir, nil)

	_, err := loader.loadRemote("github.com/someone/parser", "parser")
	if err == nil {
		t.Fatal("expected error for remote module with nil config")
	}
	if !strings.Contains(err.Error(), "no pin") {
		t.Errorf("expected 'no pin' error, got: %v", err)
	}
}

// TestIsTopLevelPin verifies the helper correctly identifies top-level pins.
func TestIsTopLevelPin(t *testing.T) {
	cfg := &module.Config{
		Name:  "myproject",
		Epoch: "2026.0",
		Require: map[string]string{
			"github.com/someone/parser":            "abc123",
			"https://github.com/someone/utils.git": "def456",
		},
		Replace: map[string]string{},
	}

	loader := testModuleLoaderWithConfig(t.TempDir(), cfg)

	// Exact match
	if !loader.isTopLevelPin("github.com/someone/parser") {
		t.Error("expected github.com/someone/parser to be a top-level pin")
	}

	// Normalized match (scheme + .git stripped)
	if !loader.isTopLevelPin("github.com/someone/utils") {
		t.Error("expected github.com/someone/utils to match normalized top-level pin")
	}

	// Not a top-level pin
	if loader.isTopLevelPin("github.com/other/lib") {
		t.Error("expected github.com/other/lib to NOT be a top-level pin")
	}

	// Nil config
	loaderNil := testModuleLoaderWithConfig(t.TempDir(), nil)
	if loaderNil.isTopLevelPin("github.com/someone/parser") {
		t.Error("expected false with nil config")
	}
}

// TestLoadRemoteModulePinConflict verifies that conflicting transitive pins produce an error.
func TestLoadRemoteModulePinConflict(t *testing.T) {
	projectDir := t.TempDir()

	// Create two local modules that will be used via [replace].
	// Module A's promise.toml has [require] pinning "shared" to commit "aaa..."
	modA := filepath.Join(projectDir, "mod_a")
	os.MkdirAll(modA, 0755)
	os.WriteFile(filepath.Join(modA, "promise.toml"), []byte(`
[module]
name = "mod_a"
epoch = "2026.0"

[require]
"github.com/shared/lib" = "aaaaaaa"
`), 0644)
	os.WriteFile(filepath.Join(modA, "a.pr"), []byte(`
a_func() int `+"`"+`public { return 1; }
`), 0644)

	// Module B's promise.toml pins "shared" to a DIFFERENT commit "bbb..."
	modB := filepath.Join(projectDir, "mod_b")
	os.MkdirAll(modB, 0755)
	os.WriteFile(filepath.Join(modB, "promise.toml"), []byte(`
[module]
name = "mod_b"
epoch = "2026.0"

[require]
"github.com/shared/lib" = "bbbbbbb"
`), 0644)
	os.WriteFile(filepath.Join(modB, "b.pr"), []byte(`
b_func() int `+"`"+`public { return 2; }
`), 0644)

	// Root project uses both via [replace]
	cfg := &module.Config{
		Name:    "myproject",
		Epoch:   "2026.0",
		Dir:     projectDir,
		Require: map[string]string{},
		Replace: map[string]string{
			"github.com/someone/mod_a": "./mod_a",
			"github.com/someone/mod_b": "./mod_b",
		},
	}

	loader := testModuleLoaderWithConfig(projectDir, cfg)

	// Load A — should succeed and register its pin for shared/lib
	_, err := loader.loadRemote("github.com/someone/mod_a", "mod_a")
	if err != nil {
		t.Fatalf("loadRemote mod_a: %v", err)
	}

	// Verify the transitive pin was registered
	sharedNorm := module.NormalizeURL("github.com/shared/lib")
	if pin, ok := loader.commitPins[sharedNorm]; !ok || pin != "aaaaaaa" {
		t.Errorf("expected commitPins[shared/lib] = 'aaaaaaa', got %q (ok=%v)", pin, ok)
	}

	// Load B — should fail because it pins shared/lib to a different commit
	_, err = loader.loadRemote("github.com/someone/mod_b", "mod_b")
	if err == nil {
		t.Fatal("expected error for conflicting pins")
	}
	if !strings.Contains(err.Error(), "conflicting pins") {
		t.Errorf("expected 'conflicting pins' error, got: %v", err)
	}
}

// TestLoadRemoteModulePinConflictTopLevelOverride verifies that a top-level [require]
// pin overrides a transitive conflict.
func TestLoadRemoteModulePinConflictTopLevelOverride(t *testing.T) {
	projectDir := t.TempDir()

	// Module A pins shared/lib to "aaaaaaa"
	modA := filepath.Join(projectDir, "mod_a")
	os.MkdirAll(modA, 0755)
	os.WriteFile(filepath.Join(modA, "promise.toml"), []byte(`
[module]
name = "mod_a"
epoch = "2026.0"

[require]
"github.com/shared/lib" = "aaaaaaa"
`), 0644)
	os.WriteFile(filepath.Join(modA, "a.pr"), []byte(`
a_func() int `+"`"+`public { return 1; }
`), 0644)

	// Module B pins shared/lib to a DIFFERENT commit "bbbbbbb"
	modB := filepath.Join(projectDir, "mod_b")
	os.MkdirAll(modB, 0755)
	os.WriteFile(filepath.Join(modB, "promise.toml"), []byte(`
[module]
name = "mod_b"
epoch = "2026.0"

[require]
"github.com/shared/lib" = "bbbbbbb"
`), 0644)
	os.WriteFile(filepath.Join(modB, "b.pr"), []byte(`
b_func() int `+"`"+`public { return 2; }
`), 0644)

	// Root project explicitly pins shared/lib — this should override both
	cfg := &module.Config{
		Name:  "myproject",
		Epoch: "2026.0",
		Dir:   projectDir,
		Require: map[string]string{
			"github.com/shared/lib": "ccccccc",
		},
		Replace: map[string]string{
			"github.com/someone/mod_a": "./mod_a",
			"github.com/someone/mod_b": "./mod_b",
		},
	}

	loader := testModuleLoaderWithConfig(projectDir, cfg)

	// Load A — should succeed, its pin for shared/lib is overridden by top-level
	_, err := loader.loadRemote("github.com/someone/mod_a", "mod_a")
	if err != nil {
		t.Fatalf("loadRemote mod_a: %v", err)
	}

	// Load B — should also succeed because top-level overrides both
	_, err = loader.loadRemote("github.com/someone/mod_b", "mod_b")
	if err != nil {
		t.Fatalf("loadRemote mod_b should succeed with top-level override: %v", err)
	}

	// The effective pin should be the top-level one
	sharedNorm := module.NormalizeURL("github.com/shared/lib")
	if pin := loader.commitPins[sharedNorm]; pin != "ccccccc" {
		t.Errorf("expected top-level pin 'ccccccc', got %q", pin)
	}
}

// TestNamedRequireDispatch verifies that a [require.NAME] entry routes
// through loadRemote with the correct URL (using [replace] for local resolution).
func TestNamedRequireDispatch(t *testing.T) {
	projectDir := t.TempDir()

	// Create local module directory
	modDir := filepath.Join(projectDir, "local-parser")
	os.MkdirAll(modDir, 0755)
	os.WriteFile(filepath.Join(modDir, "promise.toml"), []byte(`
[module]
name = "parser"
epoch = "2026.0"
`), 0644)
	os.WriteFile(filepath.Join(modDir, "parser.pr"), []byte(`
parse(int x) int `+"`"+`public { return x + 1; }
`), 0644)

	cfg := &module.Config{
		Name:    "myproject",
		Epoch:   "2026.0",
		Dir:     projectDir,
		Require: map[string]string{},
		NamedRequire: map[string]*module.RequireEntry{
			"parser": {
				URL:    "https://github.com/alice/parser",
				Commit: "a1b2c3d",
			},
		},
		Replace: map[string]string{
			"github.com/alice/parser": "./local-parser",
		},
	}

	loader := testModuleLoaderWithConfig(projectDir, cfg)

	// Verify namedRequire is populated
	if entry, ok := loader.namedRequire["parser"]; !ok {
		t.Fatal("namedRequire[parser] not populated")
	} else if entry.URL != "https://github.com/alice/parser" {
		t.Errorf("namedRequire[parser].URL = %q", entry.URL)
	}

	// Verify commit pin was added from named require entry
	norm := module.NormalizeURL("https://github.com/alice/parser")
	if pin := loader.commitPins[norm]; pin != "a1b2c3d" {
		t.Errorf("commitPins[%s] = %q, want %q", norm, pin, "a1b2c3d")
	}

	// Simulate the dispatch: loadRemote with the named require URL
	// (same call loadModuleScopes/loadDeps would make)
	modInfo, err := loader.loadRemote("https://github.com/alice/parser", "parser")
	if err != nil {
		t.Fatalf("loadRemote via named require: %v", err)
	}
	if modInfo == nil {
		t.Fatal("expected non-nil ModuleInfo")
	}
	if modInfo.CanonicalName != "parser" {
		t.Errorf("CanonicalName = %q, want %q", modInfo.CanonicalName, "parser")
	}
}

// TestNamedRequireCommitPins verifies that named require entries contribute
// to commitPins the same way as URL-keyed [require] entries.
func TestNamedRequireCommitPins(t *testing.T) {
	projectDir := t.TempDir()

	cfg := &module.Config{
		Name:  "myproject",
		Epoch: "2026.0",
		Dir:   projectDir,
		Require: map[string]string{
			"github.com/foo/bar": "aaa1111",
		},
		NamedRequire: map[string]*module.RequireEntry{
			"parser": {
				URL:    "https://github.com/alice/parser",
				Commit: "bbb2222",
			},
			"utils": {
				URL:    "git://github.com/bob/utils.git",
				Commit: "ccc3333",
			},
		},
		Replace: map[string]string{},
	}

	loader := testModuleLoaderWithConfig(projectDir, cfg)

	// All three should have normalized commit pins
	tests := []struct {
		url  string
		want string
	}{
		{"github.com/foo/bar", "aaa1111"},
		{"https://github.com/alice/parser", "bbb2222"},
		{"git://github.com/bob/utils.git", "ccc3333"},
	}
	for _, tt := range tests {
		norm := module.NormalizeURL(tt.url)
		if got := loader.commitPins[norm]; got != tt.want {
			t.Errorf("commitPins[%s] = %q, want %q", norm, got, tt.want)
		}
	}
}

// TestInvalidSyntax verifies various invalid inputs produce parse errors.
func TestInvalidSyntax(t *testing.T) {
	cases := []struct {
		name string
		code string
	}{
		// "missing_semi" removed: T0121 allows omitting `;` before `}`
		{"missing_semi_between_stmts", `main() { int x = 1 int y = 2 }`}, // `;` still required between statements
		{"unclosed_brace", `main() { Int x = 1;`},
		{"unclosed_paren", `main() { f(1, 2; }`},
		{"unclosed_bracket", `main() { Int x = a[0; }`},
		{"unclosed_string", `main() { String s = "hello; }`},
		{"double_semi", `main() { ;; }`},
		{"bare_operator", `main() { + ; }`},
		{"missing_body", `main()`},
		{"type_no_brace", `type Foo`},
		{"enum_no_brace", `enum Foo`},
		{"invalid_top_level", `42;`},
		{"assignment_no_lhs", `main() { = 42; }`},
		{"prefix_increment", `main() { int x = 0; ++x; }`},
		{"prefix_decrement", `main() { int x = 0; --x; }`},
		{"empty_match", `main() { match x {} }`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := parseString(tc.code)
			if errs == 0 {
				t.Errorf("expected errors for: %s", tc.code)
			}
		})
	}
}

func TestEpochMismatchWarning(t *testing.T) {
	projectDir := t.TempDir()
	modDir := filepath.Join(projectDir, "libs", "mymod")
	if err := os.MkdirAll(modDir, 0755); err != nil {
		t.Fatal(err)
	}

	os.WriteFile(filepath.Join(projectDir, "promise.toml"), []byte(`
[module]
name = "myapp"
epoch = "2026.0"
`), 0644)

	os.WriteFile(filepath.Join(modDir, "promise.toml"), []byte(`
[module]
name = "mymod"
epoch = "2025.1"
`), 0644)

	os.WriteFile(filepath.Join(modDir, "lib.pr"), []byte("greet() string `public { return \"hello\"; }\n"), 0644)

	projectCfg, err := module.ParseConfig(filepath.Join(projectDir, "promise.toml"))
	if err != nil {
		t.Fatal(err)
	}

	loader := testModuleLoaderWithConfig(projectDir, projectCfg)
	_, loadErr := loader.load(modDir)
	if loadErr != nil {
		t.Fatal(loadErr)
	}

	if len(loader.warnings) == 0 {
		t.Fatal("expected epoch mismatch warning, got none")
	}
	w := loader.warnings[0]
	if !strings.Contains(w, "epoch") || !strings.Contains(w, "2025.1") || !strings.Contains(w, "2026.0") {
		t.Errorf("unexpected warning: %q", w)
	}
}

func TestEpochMatchNoWarning(t *testing.T) {
	projectDir := t.TempDir()
	modDir := filepath.Join(projectDir, "libs", "mymod")
	if err := os.MkdirAll(modDir, 0755); err != nil {
		t.Fatal(err)
	}

	os.WriteFile(filepath.Join(projectDir, "promise.toml"), []byte(`
[module]
name = "myapp"
epoch = "2026.0"
`), 0644)

	os.WriteFile(filepath.Join(modDir, "promise.toml"), []byte(`
[module]
name = "mymod"
epoch = "2026.0"
`), 0644)

	os.WriteFile(filepath.Join(modDir, "lib.pr"), []byte("greet() string `public { return \"hello\"; }\n"), 0644)

	projectCfg, err := module.ParseConfig(filepath.Join(projectDir, "promise.toml"))
	if err != nil {
		t.Fatal(err)
	}

	loader := testModuleLoaderWithConfig(projectDir, projectCfg)
	_, loadErr := loader.load(modDir)
	if loadErr != nil {
		t.Fatal(loadErr)
	}

	if len(loader.warnings) != 0 {
		t.Errorf("expected no warnings for matching epochs, got: %v", loader.warnings)
	}
}

// captureStderr captures os.Stderr output produced by fn.
// Not safe to run in parallel with other tests that write to os.Stderr.
func captureStderr(fn func()) string {
	r, w, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	old := os.Stderr
	os.Stderr = w
	fn()
	w.Close()
	os.Stderr = old
	var buf bytes.Buffer
	io.Copy(&buf, r) //nolint:errcheck
	r.Close()
	return buf.String()
}

// TestErrorListenerCap verifies that errorListener caps output at 13 errors
// and always increments the error count regardless of the cap.
func TestErrorListenerCap(t *testing.T) {
	// Invoke SyntaxError N times on a listener with no real file (readFileLines
	// returns nil for a nonexistent file, so no context lines are attempted).
	invoke := func(l *errorListener, n int) {
		for i := 0; i < n; i++ {
			l.SyntaxError(nil, nil, 1, i, "syntax error", nil)
		}
	}

	t.Run("counts-all-errors-beyond-cap", func(t *testing.T) {
		l := &errorListener{filename: "nonexistent.pr"}
		captureStderr(func() { invoke(l, 20) })
		if l.errors != 20 {
			t.Errorf("errors: got %d, want 20", l.errors)
		}
	})

	t.Run("output-capped-at-13", func(t *testing.T) {
		l := &errorListener{filename: "nonexistent.pr"}
		out := captureStderr(func() { invoke(l, 20) })
		// Each of the first 13 errors emits one "nonexistent.pr:..." line.
		// Error 14 (l.errors==13 at that point) emits the suppression notice.
		// Errors 15-20 produce no output.
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		// 13 error lines + 1 suppression line = 14 non-empty lines
		if len(lines) != 14 {
			t.Errorf("stderr line count: got %d, want 14\noutput:\n%s", len(lines), out)
		}
		if !strings.Contains(out, "too many errors, suppressing remaining") {
			t.Errorf("expected suppression notice in output, got:\n%s", out)
		}
	})

	t.Run("no-suppression-when-exactly-13-errors", func(t *testing.T) {
		l := &errorListener{filename: "nonexistent.pr"}
		out := captureStderr(func() { invoke(l, 13) })
		// Exactly 13 errors: all printed, no suppression notice (14th call never happens)
		if strings.Contains(out, "too many errors") {
			t.Errorf("unexpected suppression notice for exactly 13 errors:\n%s", out)
		}
		if l.errors != 13 {
			t.Errorf("errors: got %d, want 13", l.errors)
		}
	})

	t.Run("silent-suppresses-all-output", func(t *testing.T) {
		l := &errorListener{filename: "nonexistent.pr", silent: true}
		out := captureStderr(func() { invoke(l, 20) })
		if out != "" {
			t.Errorf("silent mode: expected no output, got %q", out)
		}
		if l.errors != 20 {
			t.Errorf("errors: got %d, want 20", l.errors)
		}
	})

	t.Run("suppression-notice-no-filename-prefix", func(t *testing.T) {
		// Inline exec mode: source set, filename empty.
		// The suppression notice should not have a bare ":" prefix.
		l := &errorListener{source: "x := @"}
		out := captureStderr(func() { invoke(l, 15) })
		if strings.Contains(out, "too many errors") {
			// Must not start with ": too many" (empty filename prefix)
			if strings.Contains(out, ": too many errors") && !strings.Contains(out, "nonexistent") {
				// Check no leading colon on its own line
				for _, line := range strings.Split(out, "\n") {
					if strings.HasPrefix(line, ": too many") {
						t.Errorf("suppression notice has empty filename prefix: %q", line)
					}
				}
			}
		}
	})
}

// TestT0866EmptyBraceParses verifies that a bare `{}` in value position now
// parses cleanly (it builds to an *ast.EmptyBraceLit that sema rejects with a
// guiding error), and that the existing collection-literal forms are unaffected.
func TestT0866EmptyBraceParses(t *testing.T) {
	cases := []struct {
		name string
		code string
	}{
		{"empty_brace_var", `main() { map[string,int] m = {}; }`},
		{"empty_brace_return", `f() map[string,int] { return {}; }`},
		{"empty_brace_arg", `main() { take({}); }`},
		{"empty_map_colon", `main() { map[string,int] m = {:}; }`},
		{"populated_map", `main() { map[string,int] m = {"a": 1}; }`},
		{"empty_vector", `main() { int[] v = []; }`},
		{"empty_block_match_arm", `f(int n) { match n { 1 => {}, _ => {} } }`},
		// The matchArm grammar reorder (block before expression) must not steal
		// map-literal-valued arms: `{:}` and `{k: v}` are still expressions.
		{"map_literal_match_arm", `f(int n) map[string,int] { return match n { _ => {"a": 1} }; }`},
		{"empty_map_match_arm", `f(int n) { match n { _ => {:} } }`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if errs := parseString(tc.code); errs != 0 {
				t.Errorf("expected 0 parse errors, got %d for: %s", errs, tc.code)
			}
		})
	}
}
