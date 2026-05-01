package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antlr4-go/antlr/v4"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/parser"
	"djabi.dev/go/promise_lang/internal/sema"
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
		{"postfix_bang_plus", `main() { Int x = f()! + 1; }`},
		{"postfix_question_plus", `main() { Int x = f()? + 1; }`},
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
		{"mixed_postfix", `main() { x.foo()!.bar()?.baz; }`},
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
		{"ref_param", `main() { f := |String &s| -> s.len(); }`},
		{"chained_lambdas", `main() { a.map(|x| -> x * 2).filter(|x| -> x > 0); }`},
		{"lambda_as_arg", `main() { run(|| { io.println("hi"); }); }`},
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
		{"as_statement", `main() { match x { _ => y(), } io.println("after"); }`},
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
		{"method_instance", `type Foo { get(&this) Int ` + "`instance { return 0; } }"},
		{"method_mut", `type Foo { set(~this, Int v) ` + "`instance { this.v = v; } }"},
		{"operator_method", `type Vec { +(Vec &a, Vec &b) Vec ` + "`static { return Vec(); } }"},
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
		{"propagate", `f() Int! { Int x = g()?; return x; }`},
		{"unwrap_bang", `f() { Int x = g()!; }`},
		{"handler_named", `f() Int! { Int x = g() ? e { return 0; }; return x; }`},
		{"handler_unnamed", `f() Int! { Int x = g() ? { return 0; }; return x; }`},
		{"handler_discard", `f() Int! { Int x = g() ? _ { return 0; }; return x; }`},
		{"chained_propagate", `f() Int! { return a()?.b()?.c; }`},
		{"propagate_in_expr", `f() Int! { return g()? + 1; }`},
		{"raise", `f() Int! { raise Error("bad"); }`},
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
		{"shared_borrow_param", `f(String &s) Int { return s.len(); }`},
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
		{"inc_dec_for_loop", `main() { for i := 0; i < 10; i++ { print_int(i); } }`},
		{"dec_for_loop", `main() { for i := 10; i > 0; i-- { print_int(i); } }`},
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

// TestIsFullFile verifies the detection heuristic for full files vs expressions.
func TestIsFullFile(t *testing.T) {
	full := []struct {
		name string
		code string
	}{
		{"main_function", `main() { println("hi"); }`},
		{"main_with_helper", `add(int a, int b) int { return a + b; } main() { print_int(add(1, 2)); }`},
		{"type_decl", `type Foo { int x; } main() { Foo f = Foo(x: 1); }`},
		{"enum_decl", `enum Dir { N, S } main() { Dir d = Dir.N; }`},
		{"use_statement", `use io "std/io"; main() { io.println("hi"); }`},
		{"use_only", `use io "std/io";`},
		{"type_only", `type Point { int x; int y; }`},
		{"enum_only", `enum Color { Red, Green, Blue }`},
	}
	for _, tc := range full {
		t.Run("full/"+tc.name, func(t *testing.T) {
			if !isFullFile(tc.code) {
				t.Errorf("expected isFullFile=true for: %s", tc.code)
			}
		})
	}

	expr := []struct {
		name string
		code string
	}{
		{"bare_call", `print_int(42)`},
		{"bare_println", `println("hello")`},
		{"assignment", `x := 10; print_int(x)`},
		{"multi_statement", `x := 10; y := 20; print_int(x + y)`},
		{"if_statement", `if true { print_int(1); }`},
		{"for_loop", `for i in 0..10 { print_int(i); }`},
	}
	for _, tc := range expr {
		t.Run("expr/"+tc.name, func(t *testing.T) {
			if isFullFile(tc.code) {
				t.Errorf("expected isFullFile=false for: %s", tc.code)
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
		{"bare_call", `print_int(42)`},
		{"with_semi", `print_int(42);`},
		{"multi_statement", `x := 10; print_int(x);`},
		{"string_call", `println("hello")`},
		{"if_stmt", `if true { print_int(1); }`},
		{"for_loop", `for i := 0; i < 3; i += 1 { print_int(i); }`},
		{"for_loop_inc", `for i := 0; i < 3; i++ { print_int(i); }`},
		{"increment", `x := 0; x++;`},
		{"while_loop", `while true { break; }`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := tc.source
			if !strings.HasSuffix(source, ";") && !strings.HasSuffix(source, "}") {
				source += ";"
			}
			wrapped := "main() {\n" + source + "\n}"
			errs := parseString(wrapped)
			if errs != 0 {
				t.Errorf("wrapped code has parse errors: %s", wrapped)
			}
		})
	}
}

// --- Module loading integration tests ---

// testStdFiles parses std library from the embedded FS (resources/std/*.pr).
// Uses embedded resources rather than findStdDir() to avoid picking up stale
// files from ~/.promise/lib/std/.
func testStdFiles(t *testing.T) []*ast.File {
	t.Helper()
	entries, err := embeddedStd.ReadDir("resources/std")
	if err != nil {
		t.Fatalf("cannot read embedded std: %v", err)
	}
	var files []*ast.File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pr") {
			continue
		}
		data, err := embeddedStd.ReadFile("resources/std/" + e.Name())
		if err != nil {
			t.Fatalf("cannot read embedded %s: %v", e.Name(), err)
		}
		input := antlr.NewInputStream(string(data))
		lexer := parser.NewPromiseLexer(input)
		lexer.RemoveErrorListeners()
		stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
		p := parser.NewPromiseParser(stream)
		p.RemoveErrorListeners()
		tree := p.CompilationUnit()
		f, buildErrs := ast.Build(e.Name(), tree)
		if len(buildErrs) > 0 {
			t.Fatalf("AST build errors in embedded %s: %v", e.Name(), buildErrs)
		}
		files = append(files, f)
	}
	return files
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
epoch = "2026.3"
`), 0644); err != nil {
		t.Fatal(err)
	}

	// Write module promise.toml
	if err := os.WriteFile(filepath.Join(modDir, "promise.toml"), []byte(`
[module]
name = "mymod"
epoch = "2026.3"
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
	stdFiles := testStdFiles(t)
	modInfo, err := loadLocalModule("./libs/mymod", projectDir, stdFiles)
	if err != nil {
		t.Fatalf("loadLocalModule failed: %v", err)
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
epoch = "2026.3"
`), 0644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(modDir, "promise.toml"), []byte(`
[module]
name = "mylib"
epoch = "2026.3"
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

	stdFiles := testStdFiles(t)
	modInfo, err := loadLocalModule("./mylib", projectDir, stdFiles)
	if err != nil {
		t.Fatalf("loadLocalModule failed: %v", err)
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

	_, err := loadLocalModule("./badmod", projectDir, nil)
	if err == nil {
		t.Fatal("expected error for missing promise.toml")
	}
	if !strings.Contains(err.Error(), "no promise.toml") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestLoadLocalModuleDirNotFound verifies error when module directory doesn't exist.
func TestLoadLocalModuleDirNotFound(t *testing.T) {
	projectDir := t.TempDir()
	_, err := loadLocalModule("./nonexistent", projectDir, nil)
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
epoch = "2026.3"
`), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := loadLocalModule("./empty", projectDir, nil)
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
epoch = "2026.3"
`), 0644); err != nil {
		t.Fatal(err)
	}
	// Module source with a type error: returning string where int expected
	if err := os.WriteFile(filepath.Join(modDir, "lib.pr"), []byte(`
compute() int `+"`public"+` { return "not an int"; }
`), 0644); err != nil {
		t.Fatal(err)
	}

	stdFiles := testStdFiles(t)
	_, err := loadLocalModule("./badmod", projectDir, stdFiles)
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
epoch = "2026.3"
`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modDir, "promise.toml"), []byte(`
[module]
name = "mymod"
epoch = "2026.3"
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

	stdFiles := testStdFiles(t)
	modInfo, err := loadLocalModule("./mymod", projectDir, stdFiles)
	if err != nil {
		t.Fatalf("loadLocalModule failed: %v", err)
	}
	scope := sema.ExportedScope(modInfo.SemaInfo, modInfo.File)
	if scope.Lookup("greet") == nil {
		t.Error("expected 'greet' in exported scope")
	}
	if scope.Lookup("sum") == nil {
		t.Error("expected 'sum' in exported scope")
	}
}

// TestInvalidSyntax verifies various invalid inputs produce parse errors.
func TestInvalidSyntax(t *testing.T) {
	cases := []struct {
		name string
		code string
	}{
		{"missing_semi", `main() { Int x = 1 }`},
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
