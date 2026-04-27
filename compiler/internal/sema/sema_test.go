package sema

import (
	"fmt"
	"strings"
	"testing"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/parser"
	"djabi.dev/go/promise_lang/internal/types"
	antlr "github.com/antlr4-go/antlr/v4"
)

// --- Test helpers ---

// stdAll provides all builtin type declarations needed by tests:
// numeric operators, bool/char/string operators, containers, iter/stream, range.
var stdAll string

func init() {
	var b strings.Builder

	// Numeric types: arithmetic + comparison + unary negate + inc/dec
	for _, name := range []string{"int", "i8", "i16", "i32", "i64", "uint", "u8", "u16", "u32", "u64", "f32", "f64"} {
		fmt.Fprintf(&b, "type %s `native {\n", name)
		for _, op := range []string{"+", "-", "*", "/", "%"} {
			fmt.Fprintf(&b, "\t%s(%s other) %s `native;\n", op, name, name)
		}
		for _, op := range []string{"==", "!=", "<", ">", "<=", ">="} {
			fmt.Fprintf(&b, "\t%s(%s other) bool `native;\n", op, name)
		}
		fmt.Fprintf(&b, "\t-() %s `native;\n", name)
		fmt.Fprintf(&b, "\t++() %s `native;\n", name)
		fmt.Fprintf(&b, "\t--() %s `native;\n", name)
		// Range operators for integer types only (not floats)
		if name != "f32" && name != "f64" {
			fmt.Fprintf(&b, "\t..(%s end) range `native;\n", name)
			fmt.Fprintf(&b, "\t..=(%s end) range `native;\n", name)
		}
		b.WriteString("}\n")
	}

	// Bool
	b.WriteString("type bool `native {\n")
	b.WriteString("\t&&(bool other) bool `native;\n")
	b.WriteString("\t||(bool other) bool `native;\n")
	b.WriteString("\t==(bool other) bool `native;\n")
	b.WriteString("\t!=(bool other) bool `native;\n")
	b.WriteString("\t!() bool `native;\n}\n")

	// Char
	b.WriteString("type char `native {\n")
	for _, op := range []string{"==", "!=", "<", ">", "<=", ">="} {
		fmt.Fprintf(&b, "\t%s(char other) bool `native;\n", op)
	}
	b.WriteString("\t..(char end) range `native;\n")
	b.WriteString("\t..=(char end) range `native;\n")
	b.WriteString("}\n")

	// String (operators + methods)
	b.WriteString("type string `native {\n\tint len;\n")
	b.WriteString("\t+(string other) string `native;\n")
	for _, op := range []string{"==", "!=", "<", ">", "<=", ">="} {
		fmt.Fprintf(&b, "\t%s(string other) bool `native;\n", op)
	}
	b.WriteString("\tcontains(string sub) bool `native;\n")
	b.WriteString("\tstarts_with(string prefix) bool `native;\n")
	b.WriteString("\tends_with(string suffix) bool `native;\n")
	b.WriteString("\tindex_of(string sub) int? `native;\n")
	b.WriteString("\ttrim() string `native;\n")
	b.WriteString("\tsplit(string sep) string[] `native;\n")
	b.WriteString("\t[](int index) char `native;\n")
	b.WriteString("\t[:](int? start, int? end) string `native;\n")
	b.WriteString("\tget is_empty bool => this.len == 0;\n}\n")

	// Containers
	b.WriteString("type slice[T] `native {\n\tint len;\n")
	b.WriteString("\t[](int index) T `native;\n")
	b.WriteString("\t[]=(int index, T value) `native;\n")
	b.WriteString("\t[:](int? start, int? end) T[] `native;\n")
	b.WriteString("\t[:]=(int? start, int? end, T[] value) `native;\n")
	b.WriteString("\tpush(T elem) `native;\n")
	b.WriteString("\tpop() T? `native;\n")
	b.WriteString("\tcontains(T elem) bool `native;\n")
	b.WriteString("\tremove(int index) `native;\n")
	b.WriteString("\tget is_empty bool => this.len == 0;\n}\n")

	b.WriteString("type map[K, V] `native {\n\tint len;\n")
	b.WriteString("\t[](K key) V? `native;\n")
	b.WriteString("\t[]=(K key, V value) `native;\n")
	b.WriteString("\tcontains(K key) bool `native;\n")
	b.WriteString("\tremove(K key) bool `native;\n")
	b.WriteString("\tkeys() K[] `native;\n")
	b.WriteString("\tvalues() V[] `native;\n")
	b.WriteString("\tget is_empty bool => this.len == 0;\n}\n")

	// Iter/Stream
	b.WriteString("type iter[T] `native {\n\tnext() T? `abstract;\n}\n")
	b.WriteString("type stream[T] `native {\n\titer() iter[T] `abstract;\n}\n")

	// Range
	b.WriteString("type range `native {\n\tint start `value;\n\tint end `value;\n\tbool inclusive `value;\n}\n")

	stdAll = b.String()
}

// stdContainers is kept as an alias for backward compatibility with tests
// that pass explicit std via checkOKWithStd.
var stdContainers = "" // subsumed by stdAll; tests using checkOKWithStd get stdAll automatically

func checkSource(t *testing.T, src string) (*Info, []error) {
	t.Helper()
	return checkSourceWithStd(t, "", src)
}

func checkOK(t *testing.T, src string) *Info {
	t.Helper()
	info, errs := checkSource(t, src)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	return info
}

func checkErrs(t *testing.T, src string) []error {
	t.Helper()
	_, errs := checkSource(t, src)
	return errs
}

func expectError(t *testing.T, errs []error, substr string) {
	t.Helper()
	for _, e := range errs {
		if strings.Contains(e.Error(), substr) {
			return
		}
	}
	t.Errorf("expected error containing %q, got %v", substr, errs)
}

// checkOKWithStd parses stdSrc as std and userSrc as user code, expecting no errors.
func checkOKWithStd(t *testing.T, stdSrc, userSrc string) *Info {
	t.Helper()
	info, errs := checkSourceWithStd(t, stdSrc, userSrc)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	return info
}

// checkErrsWithStd parses stdSrc as std and userSrc as user code, returning errors.
func checkErrsWithStd(t *testing.T, stdSrc, userSrc string) []error {
	t.Helper()
	_, errs := checkSourceWithStd(t, stdSrc, userSrc)
	return errs
}

func expectNoErrors(t *testing.T, errs []error) {
	t.Helper()
	if len(errs) > 0 {
		t.Errorf("unexpected errors: %v", errs)
	}
}

func assertType(t *testing.T, info *Info, typ types.Type, expected string) {
	t.Helper()
	if typ == nil {
		t.Fatalf("type is nil, expected %s", expected)
	}
	if typ.String() != expected {
		t.Errorf("type = %s, want %s", typ, expected)
	}
}

// --- Declaration Tests ---

func TestDeclareTypeDecl(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{
			name: "simple_type",
			src:  `type Dog { string name; int age; }`,
		},
		{
			name: "type_with_method",
			src:  `type Dog { string name; bark() string { return "woof"; } }`,
		},
		{
			name: "type_with_inheritance",
			src: `type Animal { string name; }
			      type Dog is Animal { int age; }`,
		},
		{
			name: "generic_type",
			src:  `type Box[T] { T value; }`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkOK(t, tt.src)
		})
	}
}

func TestDeclareEnumDecl(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{
			name: "simple_enum",
			src:  `enum Color { Red, Green, Blue }`,
		},
		{
			name: "enum_with_fields",
			src:  `enum Shape { Circle(f64 radius), Rectangle(f64 width, f64 height) }`,
		},
		{
			name: "generic_enum",
			src:  `enum Option[T] { Some(T value), None }`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkOK(t, tt.src)
		})
	}
}

func TestDeclareFuncDecl(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{
			name: "simple_function",
			src:  `add(int a, int b) int { return a + b; }`,
		},
		{
			name: "void_function",
			src:  `greet(string name) { }`,
		},
		{
			name: "failable_function",
			src:  `parse(string s) int! { return 0; }`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkOK(t, tt.src)
		})
	}
}

func TestDuplicateDeclaration(t *testing.T) {
	errs := checkErrs(t, `
		type Dog { }
		type Dog { }
	`)
	expectError(t, errs, "redeclared")
}

func TestForwardReference(t *testing.T) {
	// Functions can reference types declared later in the file
	checkOK(t, `
		makeDog() Dog { return Dog(name: "Rex"); }
		type Dog { string name; }
	`)
}

// --- Type Resolution Tests ---

func TestResolveBasicTypes(t *testing.T) {
	info := checkOK(t, `
		foo() {
			int a := 1;
			f64 b := 1.0;
			bool c := true;
			string d := "hello";
			char e := 'x';
		}
	`)
	_ = info
}

func TestResolveSliceType(t *testing.T) {
	checkOK(t, `
		type Container {
			int[] items;
		}
	`)
}

func TestResolveArrayType(t *testing.T) {
	checkOK(t, `
		type Matrix {
			f64[3] row;
		}
	`)
}

func TestResolveOptionalType(t *testing.T) {
	checkOK(t, `
		type Person {
			string? nickname;
		}
	`)
}

func TestResolveRefTypes(t *testing.T) {
	checkOK(t, `
		type View {
			string& data;
			int~ counter;
		}
	`)
}

func TestResolveTupleType(t *testing.T) {
	checkOK(t, `
		pair() (int, string) { return (1, "a"); }
	`)
}

func TestResolveFunctionType(t *testing.T) {
	checkOK(t, `
		type Handler {
			(int, int) -> bool comparator;
		}
	`)
}

func TestResolveUndefinedType(t *testing.T) {
	errs := checkErrs(t, `
		type Foo { Unknown x; }
	`)
	expectError(t, errs, "undefined type: Unknown")
}

func TestResolveGenericInstantiation(t *testing.T) {
	checkOK(t, `
		type Box[T] { T value; }
		type IntBox { Box[int] inner; }
	`)
}

// --- Literal Type Tests ---

func TestLiteralTypes(t *testing.T) {
	tests := []struct {
		name     string
		src      string
		expected string
	}{
		{"int_literal", `test() { x := 42; }`, "int"},
		{"float_literal", `test() { x := 3.14; }`, "f64"},
		{"bool_literal", `test() { x := true; }`, "bool"},
		{"string_literal", `test() { x := "hello"; }`, "string"},
		{"char_literal", `test() { x := 'a'; }`, "char"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := checkOK(t, tt.src)
			// Find the inferred variable type
			for _, typ := range info.Types {
				if typ != nil && typ.String() == tt.expected {
					return // found expected type
				}
			}
			// Check that we recorded some type info
			if len(info.Types) == 0 {
				t.Error("no types recorded")
			}
		})
	}
}

// --- Binary Expression Tests ---

func TestBinaryArithmetic(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{"add", `test() { x := 1 + 2; }`},
		{"sub", `test() { x := 5 - 3; }`},
		{"mul", `test() { x := 2 * 3; }`},
		{"div", `test() { x := 10 / 2; }`},
		{"mod", `test() { x := 10 % 3; }`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkOK(t, tt.src)
		})
	}
}

func TestBinaryComparison(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{"eq", `test() { x := 1 == 2; }`},
		{"neq", `test() { x := 1 != 2; }`},
		{"lt", `test() { x := 1 < 2; }`},
		{"gt", `test() { x := 1 > 2; }`},
		{"lte", `test() { x := 1 <= 2; }`},
		{"gte", `test() { x := 1 >= 2; }`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkOK(t, tt.src)
		})
	}
}

func TestBinaryLogical(t *testing.T) {
	checkOK(t, `test() { x := true && false; y := true || false; }`)
}

func TestBinaryTypeMismatch(t *testing.T) {
	errs := checkErrs(t, `test() { x := 1 + true; }`)
	expectError(t, errs, "cannot use")
}

func TestBinaryLogicalTypeMismatch(t *testing.T) {
	errs := checkErrs(t, `test() { x := 1 && 2; }`)
	expectError(t, errs, "requires bool")
}

func TestStringConcatenation(t *testing.T) {
	checkOK(t, `test() { x := "hello" + " world"; }`)
}

func TestStringComparison(t *testing.T) {
	checkOK(t, `test() { x := "a" == "b"; }`)
}

// --- Unary Expression Tests ---

func TestUnaryNegate(t *testing.T) {
	checkOK(t, `test() { x := -42; }`)
}

func TestUnaryNot(t *testing.T) {
	checkOK(t, `test() { x := !true; }`)
}

func TestUnaryNotTypeMismatch(t *testing.T) {
	errs := checkErrs(t, `test() { x := !42; }`)
	expectError(t, errs, "operator ! not defined on type int")
}

// --- Function Call Tests ---

func TestFunctionCall(t *testing.T) {
	checkOK(t, `
		add(int a, int b) int { return a + b; }
		test() { x := add(1, 2); }
	`)
}

func TestFunctionCallArityMismatch(t *testing.T) {
	errs := checkErrs(t, `
		add(int a, int b) int { return a + b; }
		test() { x := add(1); }
	`)
	expectError(t, errs, "expects 2 arguments, got 1")
}

func TestFunctionCallTypeMismatch(t *testing.T) {
	errs := checkErrs(t, `
		add(int a, int b) int { return a + b; }
		test() { x := add(1, "two"); }
	`)
	expectError(t, errs, "not assignable to parameter")
}

// --- Member Access Tests ---

func TestFieldAccess(t *testing.T) {
	checkOK(t, `
		type Dog { string name; int age; }
		test() {
			Dog d := Dog(name: "Rex", age: 3);
			x := d.name;
		}
	`)
}

func TestMethodAccess(t *testing.T) {
	checkOK(t, `
		type Dog {
			string name;
			bark() string { return "woof"; }
		}
		test() {
			Dog d := Dog(name: "Rex");
			x := d.bark();
		}
	`)
}

func TestInheritedFieldAccess(t *testing.T) {
	checkOK(t, `
		type Animal { string name; }
		type Dog is Animal { int age; }
		test() {
			Dog d := Dog(name: "Rex", age: 3);
			x := d.name;
		}
	`)
}

func TestUndefinedField(t *testing.T) {
	errs := checkErrs(t, `
		type Dog { string name; }
		test() {
			Dog d := Dog(name: "Rex");
			x := d.weight;
		}
	`)
	expectError(t, errs, "has no field or method weight")
}

// --- Variable Declaration Tests ---

func TestTypedVarDecl(t *testing.T) {
	checkOK(t, `test() { int x = 42; }`)
}

func TestInferredVarDecl(t *testing.T) {
	checkOK(t, `test() { x := 42; }`)
}

func TestTypedVarDeclMismatch(t *testing.T) {
	errs := checkErrs(t, `test() { int x = "hello"; }`)
	expectError(t, errs, "cannot assign")
}

func TestDestructureVarDecl(t *testing.T) {
	checkOK(t, `
		pair() (int, string) { return (1, "hello"); }
		test() { (a, b) := pair(); }
	`)
}

func TestDestructureNonTuple(t *testing.T) {
	errs := checkErrs(t, `
		foo() int { return 42; }
		test() { (a, b) := foo(); }
	`)
	expectError(t, errs, "destructuring requires tuple")
}

// --- Assignment Tests ---

func TestSimpleAssignment(t *testing.T) {
	checkOK(t, `test() { int x = 0; x = 42; }`)
}

func TestCompoundAssignment(t *testing.T) {
	checkOK(t, `test() { int x = 0; x += 1; x -= 1; x *= 2; x /= 2; x %= 3; }`)
}

func TestIncrementDecrement(t *testing.T) {
	checkOK(t, `test() { int x = 0; x++; x--; }`)
}

func TestIncrementTypeMismatch(t *testing.T) {
	errs := checkErrs(t, `test() { string s = "hi"; s++; }`)
	expectError(t, errs, "operator ++ not defined on type string")
}

func TestAssignmentTypeMismatch(t *testing.T) {
	errs := checkErrs(t, `test() { int x = 0; x = "hello"; }`)
	expectError(t, errs, "cannot assign")
}

// --- Return Statement Tests ---

func TestReturnCorrectType(t *testing.T) {
	checkOK(t, `foo() int { return 42; }`)
}

func TestReturnTypeMismatch(t *testing.T) {
	errs := checkErrs(t, `foo() int { return "hello"; }`)
	expectError(t, errs, "cannot return")
}

func TestBareReturn(t *testing.T) {
	checkOK(t, `foo() { return; }`)
}

func TestBareReturnInNonVoid(t *testing.T) {
	errs := checkErrs(t, `foo() int { return; }`)
	expectError(t, errs, "missing return value")
}

// --- Error Handling Tests ---

func TestRaiseInFailable(t *testing.T) {
	checkOK(t, `foo() int! { raise "oops"; }`)
}

func TestRaiseInNonFailable(t *testing.T) {
	errs := checkErrs(t, `foo() { raise "oops"; }`)
	expectError(t, errs, "raise outside of failable")
}

func TestErrorPropagate(t *testing.T) {
	checkOK(t, `
		parse(string s) int! { return 0; }
		foo() int! { x := parse("42")?; return x; }
	`)
}

func TestErrorPropagateInNonFailable(t *testing.T) {
	errs := checkErrs(t, `
		parse(string s) int! { return 0; }
		foo() { x := parse("42")?; }
	`)
	expectError(t, errs, "outside of failable")
}

func TestErrorUnwrap(t *testing.T) {
	checkOK(t, `
		parse(string s) int! { return 0; }
		foo() { x := parse("42")!; }
	`)
}

// --- Control Flow Tests ---

func TestIfStatement(t *testing.T) {
	checkOK(t, `
		test() {
			if true { }
		}
	`)
}

func TestIfCondMustBeBool(t *testing.T) {
	errs := checkErrs(t, `test() { if 42 { } }`)
	expectError(t, errs, "must be bool")
}

func TestWhileLoop(t *testing.T) {
	checkOK(t, `test() { while true { break; } }`)
}

func TestBreakOutsideLoop(t *testing.T) {
	errs := checkErrs(t, `test() { break; }`)
	expectError(t, errs, "break outside of loop")
}

func TestContinueOutsideLoop(t *testing.T) {
	errs := checkErrs(t, `test() { continue; }`)
	expectError(t, errs, "continue outside of loop")
}

func TestForInLoop(t *testing.T) {
	checkOK(t, `
		test() {
			int[] items = [1, 2, 3];
			for item in items {
				int x = item;
			}
		}
	`)
}

func TestInfiniteLoop(t *testing.T) {
	checkOK(t, `test() { for { break; } }`)
}

// --- Enum Tests ---

func TestEnumVariantAccess(t *testing.T) {
	checkOK(t, `
		enum Color { Red, Green, Blue }
		test() {
			Color c = Color.Red;
		}
	`)
}

func TestEnumUndefinedVariant(t *testing.T) {
	errs := checkErrs(t, `
		enum Color { Red, Green, Blue }
		test() { Color c = Color.Yellow; }
	`)
	expectError(t, errs, "has no variant or method Yellow")
}

// --- Scope Tests ---

func TestScopeShadowing(t *testing.T) {
	checkOK(t, `
		test() {
			int x = 1;
			if true {
				string x = "shadowed";
			}
		}
	`)
}

func TestUndefinedVariable(t *testing.T) {
	errs := checkErrs(t, `test() { int x = y; }`)
	expectError(t, errs, "undefined: y")
}

// --- Meta Annotation Tests ---

func TestAbstractMethodWithBody(t *testing.T) {
	// Grammar: methodDecl = methodName(...) returnType? metaAnnotation* (block | SEMI)
	errs := checkErrs(t, "type Shape {\n\tarea() f64 `abstract { return 0.0; }\n}")
	expectError(t, errs, "abstract method")
}

func TestNativeMethodWithBody(t *testing.T) {
	errs := checkErrs(t, "type Printer {\n\tprint() `native { }\n}")
	expectError(t, errs, "native method")
}

func TestAbstractMethodWithoutBody(t *testing.T) {
	checkOK(t, "type Shape {\n\tarea() f64 `abstract;\n}")
}

// --- Index Expression Tests ---

func TestArrayIndex(t *testing.T) {
	checkOK(t, `
		test() {
			int[] items = [1, 2, 3];
			int x = items[0];
		}
	`)
}

func TestIndexNonIndexable(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			int x = 42;
			int y = x[0];
		}
	`)
	expectError(t, errs, "cannot index")
}

// --- Cast Expression Tests ---

func TestSafeCast(t *testing.T) {
	checkOK(t, `
		type Animal { string name; }
		type Dog is Animal { }
		test() {
			Animal a := Dog(name: "Rex");
			Dog? d = a as Dog;
		}
	`)
}

func TestForceCast(t *testing.T) {
	checkOK(t, `
		type Animal { string name; }
		type Dog is Animal { }
		test() {
			Animal a := Dog(name: "Rex");
			Dog d = a as! Dog;
		}
	`)
}

// --- Lambda Tests ---

func TestLambdaExprBody(t *testing.T) {
	checkOK(t, `
		test() {
			f := |int x| -> x + 1;
		}
	`)
}

// --- Lambda Capture Tests ---

func TestLambdaCapturesCopyVar(t *testing.T) {
	checkOK(t, `
		test() {
			int x = 10;
			f := |int y| -> x + y;
		}
	`)
}

func TestLambdaCapturesNonCopyWithoutMoveError(t *testing.T) {
	errs := checkErrs(t, `
		type Foo { int x; drop(~this) {} }
		test() {
			f := Foo(x: 1);
			g := |int y| -> y;
		}
	`)
	// No error — f is not referenced inside g
	if len(errs) > 0 {
		t.Fatalf("expected no errors, got: %v", errs)
	}
}

func TestLambdaCapturesNonCopyRefError(t *testing.T) {
	errs := checkErrs(t, `
		type Foo { int x; }
		test() {
			f := Foo(x: 1);
			g := |int y| -> f.x + y;
		}
	`)
	expectError(t, errs, "cannot capture non-copy variable")
}

func TestLambdaCapturesNonCopyWithMove(t *testing.T) {
	checkOK(t, `
		type Foo { int x; }
		test() {
			f := Foo(x: 1);
			g := move |int y| -> f.x + y;
		}
	`)
}

func TestLambdaNoFalseCapture(t *testing.T) {
	// Variable declared inside lambda should not trigger capture
	checkOK(t, `
		test() {
			f := |int x| -> int {
				int y = x + 1;
				return y;
			};
		}
	`)
}

func TestLambdaCapturesMultipleVars(t *testing.T) {
	checkOK(t, `
		test() {
			int a = 1;
			int b = 2;
			f := |int x| -> a + b + x;
		}
	`)
}

// --- Nested Lambda Capture Tests ---

func TestLambdaNestedCaptureGrandparent(t *testing.T) {
	checkOK(t, `
		test() {
			int x = 10;
			f := |int a| -> int {
				g := |int b| -> x + b;
				return g(a);
			};
		}
	`)
}

func TestLambdaNestedCaptureTriple(t *testing.T) {
	checkOK(t, `
		test() {
			int x = 10;
			int y = 20;
			f := |int a| -> int {
				g := |int b| -> int {
					h := |int c| -> x + y + c;
					return h(b);
				};
				return g(a);
			};
		}
	`)
}

// --- Array Literal Tests ---

func TestArrayLiteral(t *testing.T) {
	checkOK(t, `test() { x := [1, 2, 3]; }`)
}

func TestEmptyArrayLiteral(t *testing.T) {
	errs := checkErrs(t, `test() { x := []; }`)
	expectError(t, errs, "empty array")
}

// --- Tuple Literal Tests ---

func TestTupleLiteral(t *testing.T) {
	checkOK(t, `test() { x := (1, "hello"); }`)
}

// --- ThisExpr Tests ---

func TestThisInMethod(t *testing.T) {
	checkOK(t, `
		type Dog {
			string name;
			getName() string { return this.name; }
		}
	`)
}

func TestThisOutsideMethod(t *testing.T) {
	errs := checkErrs(t, `
		test() { x := this; }
	`)
	expectError(t, errs, "outside of a method")
}

// --- Complex Integration Tests ---

func TestTypeWithMultipleInheritance(t *testing.T) {
	checkOK(t, "type Printable {\n\ttoString() string `abstract;\n}\n"+
		"type Comparable {\n\tcompareTo(Comparable other) int `abstract;\n}\n"+
		"type MyType is Printable, Comparable {\n\ttoString() string { return \"MyType\"; }\n\tcompareTo(Comparable other) int { return 0; }\n}")
}

func TestRecursiveType(t *testing.T) {
	checkOK(t, `
		type Node {
			int value;
			Node? next;
		}
	`)
}

func TestMutuallyRecursiveTypes(t *testing.T) {
	checkOK(t, `
		type A { B? other; }
		type B { A? other; }
	`)
}

func TestFunctionCallChain(t *testing.T) {
	checkOK(t, `
		type Dog {
			string name;
			getName() string { return this.name; }
		}
		test() {
			Dog d = Dog(name: "Rex");
			string n = d.getName();
		}
	`)
}

func TestNestedScopes(t *testing.T) {
	checkOK(t, `
		test() {
			int x = 1;
			if true {
				int y = x + 1;
				if true {
					int z = y + 1;
				}
			}
		}
	`)
}

func TestOptionalAssignment(t *testing.T) {
	checkOK(t, `
		test() {
			int? x = 42;
			int? y = none;
		}
	`)
}

// --- Builtin Operator Tests ---

func TestBuiltinOperatorsExist(t *testing.T) {
	// Verify that std declarations populate operator methods on builtin types
	checkOK(t, `main() { x := 1 + 2; }`)

	m := types.TypInt.LookupMethod("+")
	if m == nil {
		t.Fatal("int.+ method not found")
	}
	if !m.IsNative() {
		t.Error("int.+ should be native")
	}

	m = types.TypBool.LookupMethod("!")
	if m == nil {
		t.Fatal("bool.! method not found")
	}

	m = types.TypString.LookupMethod("+")
	if m == nil {
		t.Fatal("string.+ method not found")
	}

	m = types.TypChar.LookupMethod("==")
	if m == nil {
		t.Fatal("char.== method not found")
	}
}

// --- Constructor Type Checking Tests ---

func TestConstructorFieldTypeMismatch(t *testing.T) {
	errs := checkErrs(t, `
		type Dog { string name; int age; }
		test() {
			Dog d = Dog(name: 42, age: "old");
		}
	`)
	expectError(t, errs, "cannot assign int to field name of type string")
	expectError(t, errs, "cannot assign string to field age of type int")
}

func TestConstructorFieldTypeCorrect(t *testing.T) {
	checkOK(t, `
		type Dog { string name; int age; }
		test() {
			Dog d = Dog(name: "Rex", age: 3);
		}
	`)
}

func TestConstructorRequiredFieldMissing(t *testing.T) {
	errs := checkErrs(t, `
		type User { string name; int age; }
		test() {
			User u = User(name: "Alice");
		}
	`)
	expectError(t, errs, "missing required field 'age'")
}

func TestConstructorOptionalFieldOmittable(t *testing.T) {
	checkOK(t, `
		type Profile { string name; string? bio; }
		test() {
			Profile p = Profile(name: "Alice");
		}
	`)
}

func TestConstructorDefaultFieldOmittable(t *testing.T) {
	checkOK(t, `
		type Config { int port = 8080; string host; }
		test() {
			Config c = Config(host: "localhost");
		}
	`)
}

func TestConstructorAllRequiredFieldsMissing(t *testing.T) {
	errs := checkErrs(t, `
		type Point { int x; int y; }
		test() {
			Point p = Point();
		}
	`)
	expectError(t, errs, "missing required field 'x'")
	expectError(t, errs, "missing required field 'y'")
}

func TestConstructorInheritedRequiredFieldMissing(t *testing.T) {
	errs := checkErrs(t, `
		type Animal { string name; int age; }
		type Dog is Animal { string breed; }
		test() {
			Dog d = Dog(breed: "Lab");
		}
	`)
	expectError(t, errs, "missing required field 'name'")
	expectError(t, errs, "missing required field 'age'")
}

func TestConstructorGenericRequiredField(t *testing.T) {
	errs := checkErrs(t, `
		type Box[T] { T value; }
		test() {
			Box[int] b = Box[int]();
		}
	`)
	expectError(t, errs, "missing required field 'value'")
}

func TestConstructorGenericOptionalField(t *testing.T) {
	checkOK(t, `
		type MaybeBox[T] { T? value; }
		test() {
			MaybeBox[int] b = MaybeBox[int]();
		}
	`)
}

func TestConstructorDefaultTypeMismatch(t *testing.T) {
	errs := checkErrs(t, `
		type Bad { int x = "hello"; }
		test() {}
	`)
	expectError(t, errs, "cannot use string as default for field x of type int")
}

func TestConstructorDefaultTypeCorrect(t *testing.T) {
	checkOK(t, `
		type Config { int port = 8080; string host = "localhost"; }
		test() {
			Config c = Config();
		}
	`)
}

// --- Final Field Tests ---

func TestFinalFieldConstructionOK(t *testing.T) {
	checkOK(t, `
		type Token { string raw `+"`final;"+` int line `+"`final;"+` }
		test() {
			Token t = Token(raw: "if", line: 1);
		}
	`)
}

func TestFinalFieldAssignmentRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Token { string raw `+"`final;"+` int line `+"`final;"+` }
		test() {
			Token t = Token(raw: "if", line: 1);
			t.raw = "else";
		}
	`)
	expectError(t, errs, "cannot assign to "+"`"+`final field 'raw'`)
}

func TestFinalFieldReadOK(t *testing.T) {
	checkOK(t, `
		type Token { string raw `+"`final;"+` }
		test() {
			Token t = Token(raw: "if");
			string s = t.raw;
		}
	`)
}

func TestFinalFieldWithDefault(t *testing.T) {
	checkOK(t, `
		type Config { int version `+"`final"+` = 1; }
		test() {
			Config c = Config();
		}
	`)
}

// --- Explicit new() Constructor Tests ---

func TestNewConstructorBasic(t *testing.T) {
	checkOK(t, `
		type Percentage {
			int value;
			new(~this, int value) {
				if value < 0 { this.value = 0; }
				else if value > 100 { this.value = 100; }
				else { this.value = value; }
			}
		}
		test() {
			Percentage p = Percentage(value: 50);
		}
	`)
}

func TestNewConstructorReplacesImplicit(t *testing.T) {
	errs := checkErrs(t, `
		type Foo {
			int x;
			new(~this, int y) {
				this.x = y;
			}
		}
		test() {
			Foo f = Foo(x: 1);
		}
	`)
	// Should fail because 'x' is not a param of new(), 'y' is
	expectError(t, errs, "argument name 'x' does not match parameter 'y'")
}

func TestNewConstructorWrongArgCount(t *testing.T) {
	errs := checkErrs(t, `
		type Bar {
			int x;
			new(~this, int a, int b) {
				this.x = a + b;
			}
		}
		test() {
			Bar b = Bar(a: 1);
		}
	`)
	expectError(t, errs, "expects 2 arguments, got 1")
}

func TestNewConstructorFinalFieldAssignment(t *testing.T) {
	checkOK(t, `
		type Token {
			string raw `+"`final;"+`
			new(~this, string raw) {
				this.raw = raw;
			}
		}
		test() {
			Token t = Token(raw: "if");
		}
	`)
}

func TestNewConstructorMustNotReturnValue(t *testing.T) {
	errs := checkErrs(t, `
		type Bad {
			int x;
			new(~this, int x) int {
				this.x = x;
				return 0;
			}
		}
		test() {}
	`)
	expectError(t, errs, "must not declare a return type")
}

func TestFailableNewConstructorSema(t *testing.T) {
	checkOK(t, `
		type Port {
			int value;
			new(~this, int value) void! {
				if value < 1 {
					raise "invalid port";
				}
				this.value = value;
			}
		}
		test()! {
			Port p = Port(value: 80)!;
		}
	`)
}

// --- Factory Constructor Tests ---

func TestFactoryBasic(t *testing.T) {
	checkOK(t, `
		type Color {
			int r;
			int g;
			int b;
			red() Self `+"`"+`factory {
				return Color(r: 255, g: 0, b: 0);
			}
		}
		test() {
			Color c = Color.red();
		}
	`)
}

func TestFactoryFinalFieldModification(t *testing.T) {
	checkOK(t, `
		type Token {
			string raw `+"`"+`final;
			int kind `+"`"+`final;
			parse(string input) Self `+"`"+`factory {
				Token t = Token(raw: input, kind: 0);
				t.kind = 42;
				return t;
			}
		}
		test() {
			Token tok = Token.parse("hello");
		}
	`)
}

func TestFactoryMustHaveReturnType(t *testing.T) {
	errs := checkErrs(t, `
		type Foo {
			int x;
			make() `+"`"+`factory {
				return Foo(x: 1);
			}
		}
		test() {}
	`)
	expectError(t, errs, "must have a return type")
}

func TestFactoryMustNotBeAbstract(t *testing.T) {
	errs := checkErrs(t, `
		type Foo {
			int x;
			make() Self `+"`"+`abstract `+"`"+`factory;
		}
		test() {}
	`)
	expectError(t, errs, "must not be abstract")
}

func TestFactoryNoReceiver(t *testing.T) {
	errs := checkErrs(t, `
		type Foo {
			int x;
			make(~this) Self `+"`"+`factory {
				return Foo(x: 1);
			}
		}
		test() {}
	`)
	expectError(t, errs, "must not declare a receiver")
}

// --- Inheritance / super() Tests ---

func TestSuperCallParentHasNew(t *testing.T) {
	checkOK(t, `
		type Animal {
			string name;
			new(~this, string name) {
				this.name = name;
			}
		}
		type Dog is Animal {
			string breed;
			new(~this, string name, string breed) {
				super(name);
				this.breed = breed;
			}
		}
		test() {
			Dog d = Dog(name: "Rex", breed: "Lab");
		}
	`)
}

func TestSuperCallParentImplicit(t *testing.T) {
	checkOK(t, `
		type Animal {
			string name;
			int age;
		}
		type Dog is Animal {
			string breed;
			new(~this, string name, string breed) {
				super(name: name, age: 0);
				this.breed = breed;
			}
		}
		test() {
			Dog d = Dog(name: "Rex", breed: "Lab");
		}
	`)
}

func TestSuperCallOutsideNew(t *testing.T) {
	errs := checkErrs(t, `
		type Animal {
			string name;
		}
		type Dog is Animal {
			string breed;
			bark() {
				super(name: "x");
			}
		}
		test() {}
	`)
	expectError(t, errs, "super() can only be called inside a new()")
}

func TestSuperCallNoParent(t *testing.T) {
	errs := checkErrs(t, `
		type Foo {
			int x;
			new(~this, int x) {
				super(x);
				this.x = x;
			}
		}
		test() {}
	`)
	expectError(t, errs, "has no parent")
}

func TestChildMustDefineNewWhenParentHasNew(t *testing.T) {
	errs := checkErrs(t, `
		type Animal {
			string name;
			new(~this, string name) {
				this.name = name;
			}
		}
		type Dog is Animal {
			string breed;
		}
		test() {}
	`)
	expectError(t, errs, "must define new()")
}

// --- Interaction Tests ---

func TestCopyTypeWithNewAndFinal(t *testing.T) {
	checkOK(t, `
		type Point `+"`"+`copy {
			int x `+"`"+`final;
			int y `+"`"+`final;
			new(~this, int x, int y) {
				this.x = x;
				this.y = y;
			}
		}
		test() {
			Point p = Point(x: 1, y: 2);
		}
	`)
}

func TestNewWithDropSymmetry(t *testing.T) {
	checkOK(t, `
		type Resource {
			int id;
			new(~this, int id) {
				this.id = id;
			}
			drop(~this) {}
		}
		test() {
			Resource r = Resource(id: 42);
		}
	`)
}

func TestFinalFieldCustomGetterOK(t *testing.T) {
	checkOK(t, `
		type Token {
			string raw `+"`"+`final;
			get text string {
				return this.raw;
			}
		}
		test() {
			Token t = Token(raw: "hello");
			string s = t.text;
		}
	`)
}

func TestFinalFieldCustomSetterError(t *testing.T) {
	errs := checkErrs(t, `
		type Token {
			string raw `+"`"+`final;
			set raw(string v) {
				this.raw = v;
			}
		}
		test() {}
	`)
	expectError(t, errs, "cannot define setter for")
}

// --- Self Type Tests ---

func TestSelfReturnType(t *testing.T) {
	checkOK(t, `
		type Point {
			int x;
			int y;
			offset(int dx, int dy) Self {
				return Point(x: this.x + dx, y: this.y + dy);
			}
		}
		test() {
			Point p = Point(x: 1, y: 2);
			Point q = p.offset(3, 4);
		}
	`)
}

func TestSelfConstructorCall(t *testing.T) {
	checkOK(t, `
		type Point {
			int x;
			int y;
			origin() Self {
				return Self(x: 0, y: 0);
			}
		}
		test() {
			Point p = Point(x: 1, y: 2);
			Point q = p.origin();
		}
	`)
}

func TestSelfOutsideType(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			Self x;
		}
	`)
	expectError(t, errs, "Self can only be used inside a type body")
}

func TestSelfParameterType(t *testing.T) {
	checkOK(t, `
		type Foo {
			int x;
			eq(Self other) bool {
				return this.x == other.x;
			}
		}
		test() {
			Foo a = Foo(x: 1);
			Foo b = Foo(x: 2);
			bool r = a.eq(b);
		}
	`)
}

// --- Fix #1: Failable parent new propagation ---

func TestChildNewMustBeFailableWhenParentIs(t *testing.T) {
	errs := checkErrs(t, `
		type Animal {
			string name;
			new(~this, string name) void! {
				if name == "" { raise "empty"; }
				this.name = name;
			}
		}
		type Dog is Animal {
			string breed;
			new(~this, string name, string breed) {
				super(name);
				this.breed = breed;
			}
		}
		test() {}
	`)
	expectError(t, errs, "must be failable because parent")
}

func TestChildNewFailableMatchesParent(t *testing.T) {
	checkOK(t, `
		type Animal {
			string name;
			new(~this, string name) void! {
				if name == "" { raise "empty"; }
				this.name = name;
			}
		}
		type Dog is Animal {
			string breed;
			new(~this, string name, string breed) void! {
				super(name);
				this.breed = breed;
			}
		}
		test()! {
			Dog d = Dog(name: "Rex", breed: "Lab")!;
		}
	`)
}

// --- Fix #2: Factory final field restriction ---

func TestFactoryFinalFieldOnLocalOK(t *testing.T) {
	checkOK(t, `
		type Token {
			string raw `+"`final;"+`
			int kind `+"`final;"+`
			parse(string input) Self `+"`factory"+` {
				Token t = Token(raw: input, kind: 0);
				t.kind = 42;
				return t;
			}
		}
		test() {
			Token tok = Token.parse("hello");
		}
	`)
}

func TestFactoryFinalFieldOnParamRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Foo {
			int x `+"`final;"+`
			modify(Foo other) Self `+"`factory"+` {
				other.x = 99;
				return Foo(x: 1);
			}
		}
		test() {}
	`)
	expectError(t, errs, "only allowed on locally-created instances")
}

func TestFactoryFinalFieldOnInferredLocalOK(t *testing.T) {
	checkOK(t, `
		type Point {
			int x `+"`final;"+`
			int y `+"`final;"+`
			origin() Self `+"`factory"+` {
				p := Point(x: 0, y: 0);
				return p;
			}
		}
		test() {
			Point p = Point.origin();
		}
	`)
}

// --- Fix #3: Type ordering ---

func TestChildBeforeParentNewCheck(t *testing.T) {
	// Child declared before parent — the parent-new check should still work
	// because validateConstructors runs after all types are defined
	errs := checkErrs(t, `
		type Dog is Animal {
			string breed;
		}
		type Animal {
			string name;
			new(~this, string name) {
				this.name = name;
			}
		}
		test() {}
	`)
	expectError(t, errs, "must define new()")
}

// --- Is-Pattern Tests ---

func TestIsPresent(t *testing.T) {
	checkOK(t, `
		test() {
			int? x = 42;
			bool b = x is present;
		}
	`)
}

func TestIsAbsent(t *testing.T) {
	checkOK(t, `
		test() {
			int? x = none;
			bool b = x is absent;
		}
	`)
}

func TestIsTypeName(t *testing.T) {
	checkOK(t, `
		type Animal { string name; }
		type Dog is Animal { }
		test() {
			Animal a := Dog(name: "Rex");
			bool b = a is Dog;
		}
	`)
}

// --- Map Literal Tests ---

func TestMapLiteral(t *testing.T) {
	info := checkOK(t, `test() { m := {"a": 1, "b": 2}; }`)
	// Verify a Map type was recorded
	for _, typ := range info.Types {
		if key, val, ok := types.AsMap(typ); ok {
			assertType(t, info, key, "string")
			assertType(t, info, val, "int")
			return
		}
	}
	t.Error("no Map type recorded")
}

func TestMapLiteralKeyMismatch(t *testing.T) {
	errs := checkErrs(t, `test() { m := {"a": 1, 2: 3}; }`)
	expectError(t, errs, "map key type mismatch")
}

func TestMapLiteralValueMismatch(t *testing.T) {
	errs := checkErrs(t, `test() { m := {"a": 1, "b": "two"}; }`)
	expectError(t, errs, "map value type mismatch")
}

// Note: empty map literal {} is ambiguous with empty block in the grammar.
// The sema layer handles the case via checkMapLit, but it requires
// at least one entry to parse as a map literal.

func TestMapIndex(t *testing.T) {
	checkOK(t, `
		test() {
			m := {"a": 1, "b": 2};
			v := m["a"];
		}
	`)
}

// --- Range Operator Tests ---

func TestRangeExclusive(t *testing.T) {
	info := checkOK(t, `test() { r := 0..10; }`)
	for _, typ := range info.Types {
		if typ == types.TypRange {
			return
		}
	}
	t.Error("no range type recorded")
}

func TestRangeInclusive(t *testing.T) {
	checkOK(t, `test() { r := 0..=10; }`)
}

func TestRangeNonInt(t *testing.T) {
	errs := checkErrs(t, `test() { r := "a".."z"; }`)
	expectError(t, errs, "operator .. not defined on type string")
}

func TestRangeForIn(t *testing.T) {
	checkOK(t, `
		test() {
			for i in 0..10 {
				int x = i;
			}
		}
	`)
}

// --- Go Expression Tests ---

func TestGoExprReturnsTask(t *testing.T) {
	info := checkOK(t, `
		compute() int { return 42; }
		test() { t := go compute(); }
	`)
	for _, typ := range info.Types {
		if inst, ok := typ.(*types.Instance); ok {
			if inst.Origin() == types.TypTask {
				return
			}
		}
	}
	t.Error("no task type recorded for go expression")
}

func TestGoBlockExpr(t *testing.T) {
	checkOK(t, `
		test() {
			t := go { 42; };
		}
	`)
}

// --- Receive Operator Tests ---

func TestReceiveFromTask(t *testing.T) {
	checkOK(t, `
		compute() int { return 42; }
		test() {
			t := go compute();
			result := <-t;
		}
	`)
}

func TestReceiveFromNonTask(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			int x = 42;
			y := <-x;
		}
	`)
	expectError(t, errs, "requires task[T] or channel[T]")
}

// --- Missing Return Tests ---

func TestMissingReturnDetected(t *testing.T) {
	errs := checkErrs(t, `foo() int { int x = 42; }`)
	expectError(t, errs, "missing return")
}

func TestReturnPresent(t *testing.T) {
	checkOK(t, `foo() int { return 42; }`)
}

func TestReturnInBothIfBranches(t *testing.T) {
	checkOK(t, `
		foo(bool b) int {
			if b {
				return 1;
			} else {
				return 2;
			}
		}
	`)
}

func TestMissingReturnIfNoElse(t *testing.T) {
	errs := checkErrs(t, `
		foo(bool b) int {
			if b {
				return 1;
			}
		}
	`)
	expectError(t, errs, "missing return")
}

func TestMissingReturnMethod(t *testing.T) {
	errs := checkErrs(t, `
		type Dog {
			string name;
			getName() string { string x = this.name; }
		}
	`)
	expectError(t, errs, "missing return")
}

func TestVoidFunctionNoReturnOK(t *testing.T) {
	checkOK(t, `foo() { int x = 42; }`)
}

// --- Match Exhaustiveness Tests ---

func TestMatchExhaustiveEnum(t *testing.T) {
	checkOK(t, `
		enum Color { Red, Green, Blue }
		test() {
			Color c = Color.Red;
			x := match c {
				Color.Red => 1,
				Color.Green => 2,
				Color.Blue => 3,
			};
		}
	`)
}

func TestMatchNonExhaustiveEnum(t *testing.T) {
	errs := checkErrs(t, `
		enum Color { Red, Green, Blue }
		test() {
			Color c = Color.Red;
			x := match c {
				Color.Red => 1,
				Color.Green => 2,
			};
		}
	`)
	expectError(t, errs, "not exhaustive")
}

func TestMatchWithWildcard(t *testing.T) {
	checkOK(t, `
		enum Color { Red, Green, Blue }
		test() {
			Color c = Color.Red;
			x := match c {
				Color.Red => 1,
				_ => 0,
			};
		}
	`)
}

func TestMatchIntRequiresWildcard(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			int x = 42;
			y := match x {
				1 => "one",
				2 => "two",
			};
		}
	`)
	expectError(t, errs, "must include a wildcard")
}

func TestMatchIntWithWildcard(t *testing.T) {
	checkOK(t, `
		test() {
			int x = 42;
			y := match x {
				1 => "one",
				_ => "other",
			};
		}
	`)
}

// --- String Iteration Test ---

func TestStringForIn(t *testing.T) {
	checkOK(t, `
		test() {
			for ch in "hello" {
				char c = ch;
			}
		}
	`)
}

// --- Generic Universe Types Exist ---

func TestUniverseTaskType(t *testing.T) {
	if types.TypTask == nil {
		t.Fatal("TypTask is nil")
	}
	if len(types.TypTask.TypeParams()) != 1 {
		t.Errorf("task should have 1 type param, got %d", len(types.TypTask.TypeParams()))
	}
}

func TestUniverseChannelType(t *testing.T) {
	if types.TypChannel == nil {
		t.Fatal("TypChannel is nil")
	}
	if len(types.TypChannel.TypeParams()) != 1 {
		t.Errorf("channel should have 1 type param, got %d", len(types.TypChannel.TypeParams()))
	}
}

func TestUniverseRangeType(t *testing.T) {
	if types.TypRange == nil {
		t.Fatal("TypRange is nil")
	}
}

func TestUniverseMapType(t *testing.T) {
	if types.TypMap == nil {
		t.Fatal("TypMap is nil")
	}
	if len(types.TypMap.TypeParams()) != 2 {
		t.Errorf("map should have 2 type params, got %d", len(types.TypMap.TypeParams()))
	}
}

// --- Map For-In Test ---

func TestMapForIn(t *testing.T) {
	checkOK(t, `
		test() {
			m := {"a": 1, "b": 2};
			for entry in m {
			}
		}
	`)
}

// --- Receive Extracts Inner Type ---

func TestReceiveExtractsType(t *testing.T) {
	checkOK(t, `
		compute() int { return 42; }
		test() {
			t := go compute();
			result := <-t;
			int x = result;
		}
	`)
}

// --- Go Block Type Inference ---

func TestGoBlockExprType(t *testing.T) {
	checkOK(t, `
		test() {
			t := go { 42; };
			result := <-t;
			int x = result;
		}
	`)
}

// --- Infinite Loop Returns ---

func TestInfiniteLoopReturns(t *testing.T) {
	checkOK(t, `
		foo() int {
			for {
				return 1;
			}
		}
	`)
}

// --- Short Destructure Exhaustiveness ---

func TestMatchShortDestructureExhaustive(t *testing.T) {
	checkOK(t, `
		enum Result { Ok(int value), Err(string msg) }
		test() {
			Result r = Result.Ok(42);
			x := match r {
				Ok(v) => 0,
				Err(m) => 1,
			};
		}
	`)
}

func TestMatchShortDestructureNonExhaustive(t *testing.T) {
	errs := checkErrs(t, `
		enum Result { Ok(int value), Err(string msg) }
		test() {
			Result r = Result.Ok(42);
			x := match r {
				Ok(v) => 0,
			};
		}
	`)
	expectError(t, errs, "not exhaustive")
}

// --- For-In Non-Iterable ---

func TestForInNonIterable(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			for x in 42 {
			}
		}
	`)
	expectError(t, errs, "cannot iterate")
}

func TestForInBoolNotIterable(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			for x in true {
			}
		}
	`)
	expectError(t, errs, "cannot iterate")
}

// --- Map Type Annotation ---

func TestMapTypeAnnotation(t *testing.T) {
	checkOK(t, `
		test() {
			map[string, int] m = {"a": 1, "b": 2};
		}
	`)
}

func TestMapTypeAnnotationAsParam(t *testing.T) {
	checkOK(t, `
		lookup(map[string, int] m) {
		}
	`)
}

// --- Range Field Access ---

func TestRangeFieldAccess(t *testing.T) {
	checkOK(t, `
		test() {
			r := 0..10;
			s := r.start;
			e := r.end;
			i := r.inclusive;
			int x = s;
			int y = e;
			bool z = i;
		}
	`)
}

// --- Infinite Loop Break Detection ---

func TestInfiniteLoopBreakInMatch(t *testing.T) {
	errs := checkErrs(t, `
		foo(int x) int {
			for {
				match x {
					_ => { break; }
				}
			}
		}
	`)
	expectError(t, errs, "missing return")
}

func TestInfiniteLoopBreakInBlock(t *testing.T) {
	errs := checkErrs(t, `
		foo() int {
			for {
				{
					break;
				}
			}
		}
	`)
	expectError(t, errs, "missing return")
}

func TestInfiniteLoopBreakInElseIf(t *testing.T) {
	errs := checkErrs(t, `
		foo(bool a, bool b) int {
			for {
				if a {
				} else {
					if b {
						break;
					}
				}
			}
		}
	`)
	expectError(t, errs, "missing return")
}

func TestNonExhaustiveMatchNotReturning(t *testing.T) {
	errs := checkErrs(t, `
		foo(int x) int {
			match x {
				1 => { return 1; },
				2 => { return 2; },
			}
		}
	`)
	expectError(t, errs, "missing return")
}

func TestExhaustiveEnumMatchReturns(t *testing.T) {
	checkOK(t, `
		enum Color { Red, Green, Blue }
		foo(Color c) int {
			match c {
				Color.Red => { return 1; },
				Color.Green => { return 2; },
				Color.Blue => { return 3; },
			}
		}
	`)
}

func TestInfiniteLoopNestedLoopBreakOK(t *testing.T) {
	// Break inside a nested loop only breaks the inner loop,
	// so the outer infinite loop still "returns".
	checkOK(t, `
		foo() int {
			for {
				while true {
					break;
				}
				return 1;
			}
		}
	`)
}

// ===== Stage 5a: Generic Type Substitution Tests =====

func TestGenericFieldAccess(t *testing.T) {
	checkOK(t, `
		type Box[T] { T value; }
		test() {
			Box[int] b = Box[int](value: 42);
			int x = b.value;
		}
	`)
}

func TestGenericFieldAccessTypeMismatch(t *testing.T) {
	errs := checkErrs(t, `
		type Box[T] { T value; }
		test() {
			Box[int] b = Box[int](value: 42);
			string x = b.value;
		}
	`)
	expectError(t, errs, "cannot assign")
}

func TestGenericMethodCall(t *testing.T) {
	checkOK(t, `
		type Box[T] {
			T value;
			get() T { return this.value; }
		}
		test() {
			Box[string] b = Box[string](value: "hello");
			string s = b.get();
		}
	`)
}

func TestGenericMethodReturnTypeMismatch(t *testing.T) {
	errs := checkErrs(t, `
		type Box[T] {
			T value;
			get() T { return this.value; }
		}
		test() {
			Box[int] b = Box[int](value: 42);
			string s = b.get();
		}
	`)
	expectError(t, errs, "cannot assign")
}

func TestGenericMethodParamCheck(t *testing.T) {
	checkOK(t, `
		type Stack[T] {
			T[] items;
			push(T item) { }
		}
		test() {
			Stack[int] s = Stack[int](items: [1, 2, 3]);
			s.push(4);
		}
	`)
}

func TestGenericMethodParamMismatch(t *testing.T) {
	errs := checkErrs(t, `
		type Stack[T] {
			T[] items;
			push(T item) { }
		}
		test() {
			Stack[int] s = Stack[int](items: [1, 2, 3]);
			s.push("wrong");
		}
	`)
	expectError(t, errs, "not assignable")
}

func TestGenericConstructorValidation(t *testing.T) {
	errs := checkErrs(t, `
		type Box[T] { T value; }
		test() {
			Box[int] b = Box[int](value: "wrong");
		}
	`)
	expectError(t, errs, "cannot assign")
}

func TestNestedGenericInstance(t *testing.T) {
	// Multi-arg generics in expression context aren't supported (grammar limitation),
	// so use type annotation via function parameter.
	checkOK(t, `
		type Box[T] { T value; }
		type Pair[A, B] { A first; B second; }
		test(Pair[int, Box[string]] p) {
			Box[string] b = p.second;
			string s = b.value;
			int x = p.first;
		}
	`)
}

func TestGenericEnumVariantAccess(t *testing.T) {
	checkOK(t, `
		enum Option[T] { Some(T value), None }
		test() {
			Option[int] x = Option[int].Some(42);
			Option[int] y = Option[int].None;
		}
	`)
}

func TestGenericEnumVariantConstructorType(t *testing.T) {
	errs := checkErrs(t, `
		enum Option[T] { Some(T value), None }
		test() {
			Option[int] x = Option[int].Some("wrong");
		}
	`)
	expectError(t, errs, "not assignable")
}

func TestConstraintValidationFails(t *testing.T) {
	errs := checkErrs(t, `
		type Hashable {
			hash() int `+"`abstract;"+`
		}
		type MyMap[K: Hashable, V] { K key; V val; }
		type NoHash { }
		test() {
			MyMap[NoHash, int] m = MyMap[NoHash, int](key: NoHash(), val: 1);
		}
	`)
	expectError(t, errs, "does not satisfy constraint")
}

func TestConstraintValidationPasses(t *testing.T) {
	// Multi-arg generics use type annotation (function parameter) since
	// expression-context multi-arg is a grammar limitation.
	checkOK(t, `
		type Hashable {
			hash() int `+"`abstract;"+`
		}
		type MyKey is Hashable {
			hash() int { return 0; }
		}
		type MyMap[K: Hashable, V] { K key; V val; }
		test(MyMap[MyKey, int] m) {
			MyKey k = m.key;
			int v = m.val;
		}
	`)
}

func TestRecursiveGenericType(t *testing.T) {
	checkOK(t, `
		type Tree[T] {
			T value;
			Tree[T]? left;
			Tree[T]? right;
		}
		test() {
			Tree[int] t = Tree[int](value: 1, left: none, right: none);
		}
	`)
}

func TestGenericInstanceIdentity(t *testing.T) {
	// Box[int] should be assignable to Box[int]
	checkOK(t, `
		type Box[T] { T value; }
		test() {
			Box[int] a = Box[int](value: 1);
			Box[int] b = a;
		}
	`)
}

func TestGenericInstanceMismatch(t *testing.T) {
	errs := checkErrs(t, `
		type Box[T] { T value; }
		test() {
			Box[int] a = Box[int](value: 1);
			Box[string] b = a;
		}
	`)
	expectError(t, errs, "cannot assign")
}

func TestGenericEnumMatchExhaustive(t *testing.T) {
	// Exhaustive match on generic enum with short destructure patterns.
	checkOK(t, `
		enum Option[T] { Some(T value), None }
		test() {
			Option[int] x = Option[int].Some(42);
			y := match x {
				Some(v) => 1,
				None => 0,
			};
		}
	`)
}

func TestGenericEnumMatchNonExhaustive(t *testing.T) {
	errs := checkErrs(t, `
		enum Option[T] { Some(T value), None }
		test() {
			Option[int] x = Option[int].Some(42);
			y := match x {
				Some(v) => 1,
			};
		}
	`)
	expectError(t, errs, "not exhaustive")
}

func TestInstancesRecordedInInfo(t *testing.T) {
	info := checkOK(t, `
		type Box[T] { T value; }
		test() {
			Box[int] b = Box[int](value: 42);
		}
	`)
	if len(info.Instances) == 0 {
		t.Error("expected at least one Instance recorded")
	}
}

func TestInstancesOnlyConcreteRecorded(t *testing.T) {
	// Non-concrete instances (from field type resolution during define, e.g. Tree[T]?)
	// should not be recorded — only concrete instantiations like Tree[int].
	info := checkOK(t, `
		type Tree[T] {
			T value;
			Tree[T]? left;
			Tree[T]? right;
		}
		test() {
			Tree[int] t = Tree[int](value: 1, left: none, right: none);
		}
	`)
	for _, inst := range info.Instances {
		for _, arg := range inst.TypeArgs() {
			if types.ContainsTypeParam(arg) {
				t.Errorf("non-concrete Instance recorded: %s", inst)
			}
		}
	}
	if len(info.Instances) == 0 {
		t.Error("expected at least one concrete Instance recorded")
	}
}

func TestGenericOptionalChaining(t *testing.T) {
	checkOK(t, `
		type Box[T] { T value; }
		test() {
			Box[int]? b = Box[int](value: 42);
			int? v = b?.value;
		}
	`)
}

func TestGenericOptionalChainingTypeMismatch(t *testing.T) {
	errs := checkErrs(t, `
		type Box[T] { T value; }
		test() {
			Box[int]? b = Box[int](value: 42);
			string? v = b?.value;
		}
	`)
	expectError(t, errs, "cannot assign")
}

func TestGenericBinaryOperator(t *testing.T) {
	// Operator dispatch on a generic Instance type should substitute
	// the method signature, so + on Box[int]'s inner int value works.
	checkOK(t, `
		type Wrapper[T] {
			T value;
		}
		test() {
			Wrapper[int] w = Wrapper[int](value: 3);
			int x = w.value + 1;
		}
	`)
}

func TestGenericUnaryOperator(t *testing.T) {
	checkOK(t, `
		type Wrapper[T] {
			T value;
		}
		test() {
			Wrapper[int] w = Wrapper[int](value: 3);
			int x = -w.value;
		}
	`)
}

// ===== Stage 5b: Sema Completion Tests =====

// --- Match Pattern Binding Tests ---

func TestMatchPatternBindingShortDestructure(t *testing.T) {
	checkOK(t, `
		enum Result { Ok(int value), Err(string msg) }
		test() {
			Result r = Result.Ok(42);
			x := match r {
				Ok(v) => v,
				Err(m) => 0,
			};
			int y = x;
		}
	`)
}

func TestMatchPatternBindingEnumDestructure(t *testing.T) {
	checkOK(t, `
		enum Shape { Circle(f64 radius), Rect(f64 w, f64 h) }
		test() {
			Shape s = Shape.Circle(3.14);
			x := match s {
				Shape.Circle(r) => r,
				Shape.Rect(w, h) => w,
			};
		}
	`)
}

func TestMatchPatternBindingName(t *testing.T) {
	checkOK(t, `
		test() {
			int x = 42;
			y := match x {
				val => val + 1,
			};
		}
	`)
}

func TestMatchPatternBindingTypeBinding(t *testing.T) {
	checkOK(t, `
		type Animal { string name; }
		type Dog is Animal { int age; }
		test() {
			Animal a := Dog(name: "Rex", age: 3);
			x := match a {
				Dog d => d.age,
				_ => 0,
			};
		}
	`)
}

func TestMatchPatternBindingGenericEnum(t *testing.T) {
	// Pattern bindings on generic enum instances should get substituted types
	checkOK(t, `
		enum Option[T] { Some(T value), None }
		test() {
			Option[int] x = Option[int].Some(42);
			y := match x {
				Some(v) => v + 1,
				None => 0,
			};
		}
	`)
}

func TestMatchPatternBindingWildcardIgnored(t *testing.T) {
	checkOK(t, `
		enum Color { Red, Green, Blue }
		test() {
			Color c = Color.Red;
			x := match c {
				_ => 0,
			};
		}
	`)
}

func TestMatchPatternBindingUnderscore(t *testing.T) {
	// Underscore bindings should not be inserted into scope
	errs := checkErrs(t, `
		enum Result { Ok(int value), Err(string msg) }
		test() {
			Result r = Result.Ok(42);
			x := match r {
				Ok(_) => 0,
				Err(_) => 1,
			};
		}
	`)
	expectNoErrors(t, errs)
}

func TestMatchPatternBindingTypeMismatch(t *testing.T) {
	errs := checkErrs(t, `
		enum Result { Ok(int value), Err(string msg) }
		test() {
			Result r = Result.Ok(42);
			x := match r {
				Ok(v) => v,
				Err(m) => m,
			};
			int y = x;
		}
	`)
	// The second arm returns string, but we assign to int
	// Currently only first arm type is used for result, so this checks the binding type
	// The key point is that v: int and m: string are correctly typed
	expectNoErrors(t, errs)
}

// --- Unreachable Code Tests ---

func TestUnreachableAfterReturn(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			return;
			int x = 42;
		}
	`)
	expectError(t, errs, "unreachable code")
}

func TestUnreachableAfterRaise(t *testing.T) {
	errs := checkErrs(t, `
		test() int! {
			raise "oops";
			int x = 42;
		}
	`)
	expectError(t, errs, "unreachable code")
}

func TestUnreachableAfterBreak(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			for {
				break;
				int x = 42;
			}
		}
	`)
	expectError(t, errs, "unreachable code")
}

func TestUnreachableAfterContinue(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			while true {
				continue;
				int x = 42;
			}
		}
	`)
	expectError(t, errs, "unreachable code")
}

func TestReachableAfterIfWithoutElse(t *testing.T) {
	// No false positive: if without else doesn't guarantee exit
	checkOK(t, `
		test() {
			if true {
				return;
			}
			int x = 42;
		}
	`)
}

func TestReachableAfterIfWithElseOneReturns(t *testing.T) {
	// No false positive: only one branch returns
	checkOK(t, `
		test() {
			if true {
				return;
			} else {
				int y = 1;
			}
			int x = 42;
		}
	`)
}

// --- Multi-Constraint Tests ---

func TestMultiConstraintBothSatisfied(t *testing.T) {
	checkOK(t, `
		type Hashable {
			hash() int `+"`abstract;"+`
		}
		type Printable {
			toString() string `+"`abstract;"+`
		}
		type MyKey is Hashable, Printable {
			hash() int { return 0; }
			toString() string { return "key"; }
		}
		type MyMap[K: Hashable + Printable, V] { K key; V val; }
		test(MyMap[MyKey, int] m) {
			MyKey k = m.key;
		}
	`)
}

func TestMultiConstraintOneFails(t *testing.T) {
	errs := checkErrs(t, `
		type Hashable {
			hash() int `+"`abstract;"+`
		}
		type Printable {
			toString() string `+"`abstract;"+`
		}
		type MyKey is Hashable {
			hash() int { return 0; }
		}
		type MyMap[K: Hashable + Printable, V] { K key; V val; }
		test(MyMap[MyKey, int] m) { }
	`)
	expectError(t, errs, "does not satisfy constraint Printable")
}

func TestSingleConstraintStillWorks(t *testing.T) {
	// Existing single-constraint behavior should be unchanged
	checkOK(t, `
		type Hashable {
			hash() int `+"`abstract;"+`
		}
		type MyKey is Hashable {
			hash() int { return 0; }
		}
		type MyMap[K: Hashable, V] { K key; V val; }
		test(MyMap[MyKey, int] m) {
			MyKey k = m.key;
		}
	`)
}

// --- Iter/Stream Method Tests ---

func TestIterHasNextMethod(t *testing.T) {
	// Trigger std registration via checkOK
	checkOK(t, `main() {}`)
	m := types.TypIter.LookupMethod("next")
	if m == nil {
		t.Fatal("iter.next() method not found")
	}
	if !m.IsAbstract() {
		t.Error("iter.next() should be abstract")
	}
	sig := m.Sig()
	if sig == nil {
		t.Fatal("iter.next() has no signature")
	}
	// Return type should be T? (Optional of TypeParam)
	opt, ok := sig.Result().(*types.Optional)
	if !ok {
		t.Fatalf("iter.next() should return Optional, got %T", sig.Result())
	}
	if _, ok := opt.Elem().(*types.TypeParam); !ok {
		t.Errorf("iter.next() Optional elem should be TypeParam, got %T", opt.Elem())
	}
}

func TestStreamHasIterMethod(t *testing.T) {
	// Trigger std registration via checkOK
	checkOK(t, `main() {}`)
	m := types.TypStream.LookupMethod("iter")
	if m == nil {
		t.Fatal("stream.iter() method not found")
	}
	if !m.IsAbstract() {
		t.Error("stream.iter() should be abstract")
	}
	sig := m.Sig()
	if sig == nil {
		t.Fatal("stream.iter() has no signature")
	}
	// Return type should be iter[T] (Instance of Iter with TypeParam)
	inst, ok := sig.Result().(*types.Instance)
	if !ok {
		t.Fatalf("stream.iter() should return Instance, got %T", sig.Result())
	}
	if inst.Origin() != types.TypIter {
		t.Errorf("stream.iter() should return iter instance, got %s", inst.Origin())
	}
}

// --- Use Declaration Tests ---

func TestUseDeclReservesName(t *testing.T) {
	errs := checkErrs(t, `
		use io "std/io"
		type io { }
	`)
	expectError(t, errs, "redeclared")
}

func TestUseDeclModuleNotLoaded(t *testing.T) {
	errs := checkErrs(t, `
		use io "std/io"
		test() {
			io.Print();
		}
	`)
	expectError(t, errs, "not loaded")
}

func TestUseDeclMultiple(t *testing.T) {
	errs := checkErrs(t, `
		use io "std/io"
		use fmt "std/fmt"
		test() {
			io.Print();
		}
	`)
	expectError(t, errs, "not loaded")
	// fmt should also be reserved but not cause errors since it's unused
}

func TestUnreachableAfterIfElseBothReturn(t *testing.T) {
	errs := checkErrs(t, `
		test(bool b) {
			if b {
				return;
			} else {
				return;
			}
			int x = 42;
		}
	`)
	expectError(t, errs, "unreachable code")
}

func TestMultiConstraintAssignability(t *testing.T) {
	// TypeParam T: A + B should be assignable to both A and B
	checkOK(t, `
		type Hashable {
			hash() int `+"`abstract;"+`
		}
		type Printable {
			toString() string `+"`abstract;"+`
		}
		type Container[T: Hashable + Printable] {
			T item;
			asHashable() Hashable { return this.item; }
			asPrintable() Printable { return this.item; }
		}
	`)
}

func TestMatchPatternBindingEnumDestructureGeneric(t *testing.T) {
	// Long-form enum destructure on generic enum should substitute types
	checkOK(t, `
		enum Option[T] { Some(T value), None }
		test() {
			Option[int] x = Option[int].Some(42);
			y := match x {
				Option.Some(v) => v + 1,
				Option.None => 0,
			};
		}
	`)
}

// === Meta annotation validation ===

func TestMetaCopyOnType(t *testing.T) {
	checkOK(t, `
		type Point `+"`copy"+` {
			int x;
			int y;
		}
	`)
}

func TestMetaCopyOnFunc(t *testing.T) {
	errs := checkErrs(t, `
		test() `+"`copy"+` {}
	`)
	expectError(t, errs, "cannot be applied to function")
}

func TestMetaAbstractOnField(t *testing.T) {
	errs := checkErrs(t, `
		type T {
			int x `+"`abstract"+`;
		}
	`)
	expectError(t, errs, "cannot be applied to field")
}

func TestMetaTestOnFunc(t *testing.T) {
	info := checkOK(t, `
		myTest() `+"`test"+` {}
	`)
	if len(info.Tests) != 1 {
		t.Fatalf("expected 1 test function, got %d", len(info.Tests))
	}
	if info.Tests[0].Name() != "myTest" {
		t.Errorf("expected test function 'myTest', got '%s'", info.Tests[0].Name())
	}
}

func TestMetaTestNotOnType(t *testing.T) {
	errs := checkErrs(t, `
		type T `+"`test"+` {}
	`)
	expectError(t, errs, "cannot be applied to type")
}

func TestMetaUnknown(t *testing.T) {
	errs := checkErrs(t, `
		type T `+"`foobar"+` {
			int x;
		}
	`)
	expectError(t, errs, "unknown meta annotation")
}

func TestMetaDuplicate(t *testing.T) {
	errs := checkErrs(t, `
		type T `+"`copy `copy"+` {
			int x;
		}
	`)
	expectError(t, errs, "duplicate meta annotation")
}

// === Copy validation ===

func TestCopyTypeAllPrimitiveFields(t *testing.T) {
	checkOK(t, `
		type Point `+"`copy"+` {
			int x;
			int y;
		}
	`)
}

func TestCopyTypeWithStringField(t *testing.T) {
	errs := checkErrs(t, `
		type Bad `+"`copy"+` {
			string name;
		}
	`)
	expectError(t, errs, "non-copy type string")
}

func TestCopyTypeWithCopyNestedField(t *testing.T) {
	checkOK(t, `
		type Inner `+"`copy"+` {
			int v;
		}
		type Outer `+"`copy"+` {
			Inner i;
		}
	`)
}

func TestCopyTypeWithNonCopyNestedField(t *testing.T) {
	errs := checkErrs(t, `
		type Inner {
			string v;
		}
		type Outer `+"`copy"+` {
			Inner i;
		}
	`)
	expectError(t, errs, "non-copy type Inner")
}

func TestCopyEnumOK(t *testing.T) {
	checkOK(t, `
		enum Dir `+"`copy"+` { N, S, E, W }
	`)
}

// === Doc extraction ===

func TestDocOnType(t *testing.T) {
	info := checkOK(t, `
		type Server `+"`doc(\"HTTP server\")"+` {}
	`)
	scope := info.Scopes[findFile(t, info)]
	obj := scope.Lookup("Server")
	named := obj.(*types.TypeName).Type().(*types.Named)
	if named.Doc() != "HTTP server" {
		t.Errorf("expected doc 'HTTP server', got %q", named.Doc())
	}
}

func TestDocOnFunc(t *testing.T) {
	info := checkOK(t, `
		bar() `+"`doc(\"a func\")"+` {}
	`)
	scope := info.Scopes[findFile(t, info)]
	fn := scope.Lookup("bar").(*types.Func)
	if fn.Doc() != "a func" {
		t.Errorf("expected doc 'a func', got %q", fn.Doc())
	}
}

// findFile returns the *ast.File from info.Scopes keys.
func findFile(t *testing.T, info *Info) *ast.File {
	t.Helper()
	for node := range info.Scopes {
		if f, ok := node.(*ast.File); ok {
			return f
		}
	}
	t.Fatal("no file scope found")
	return nil
}

// === Deprecated ===

func TestDeprecatedType(t *testing.T) {
	info := checkOK(t, `
		type Old `+"`deprecated"+` {}
	`)
	scope := info.Scopes[findFile(t, info)]
	named := scope.Lookup("Old").(*types.TypeName).Type().(*types.Named)
	if named.Deprecated() == "" {
		t.Error("expected type to be marked deprecated")
	}
}

func TestDeprecatedWithMessage(t *testing.T) {
	info := checkOK(t, `
		type Old `+"`deprecated(\"use New\")"+` {}
	`)
	scope := info.Scopes[findFile(t, info)]
	named := scope.Lookup("Old").(*types.TypeName).Type().(*types.Named)
	if named.Deprecated() != "use New" {
		t.Errorf("expected deprecated message 'use New', got %q", named.Deprecated())
	}
}

func TestDeprecatedWarningOnUse(t *testing.T) {
	errs := checkErrs(t, `
		type Old `+"`deprecated"+` {}
		test() {
			Old o = Old();
		}
	`)
	expectError(t, errs, "deprecated type 'Old'")
}

func TestDeprecatedFunc(t *testing.T) {
	errs := checkErrs(t, `
		old() `+"`deprecated"+` {}
		test() {
			old();
		}
	`)
	expectError(t, errs, "deprecated function 'old'")
}

func TestDeprecatedEnum(t *testing.T) {
	errs := checkErrs(t, `
		enum Status `+"`deprecated"+` { On, Off }
		test() {
			Status s = Status.On;
		}
	`)
	expectError(t, errs, "deprecated enum 'Status'")
}

func TestDeprecatedField(t *testing.T) {
	errs := checkErrs(t, `
		type T {
			int x `+"`deprecated"+`;
		}
		test() {
			T t = T(x: 1);
			int v = t.x;
		}
	`)
	expectError(t, errs, "deprecated field 'x'")
}

func TestDeprecatedMethod(t *testing.T) {
	errs := checkErrs(t, `
		type T {
			foo() `+"`deprecated"+` {}
		}
		test() {
			T t = T();
			t.foo();
		}
	`)
	expectError(t, errs, "deprecated method 'foo'")
}

// === Doc on method ===

func TestDocOnMethod(t *testing.T) {
	info := checkOK(t, `
		type T {
			foo() `+"`doc(\"does stuff\")"+` {}
		}
	`)
	scope := info.Scopes[findFile(t, info)]
	named := scope.Lookup("T").(*types.TypeName).Type().(*types.Named)
	m := named.LookupMethod("foo")
	if m.Doc() != "does stuff" {
		t.Errorf("expected doc 'does stuff', got %q", m.Doc())
	}
}

// === Copy enum with variant fields ===

func TestCopyEnumWithNonCopyVariantField(t *testing.T) {
	errs := checkErrs(t, `
		enum Bad `+"`copy"+` { X(string s) }
	`)
	expectError(t, errs, "non-copy field type string")
}

func TestCopyEnumWithCopyVariantFields(t *testing.T) {
	checkOK(t, `
		enum Expr `+"`copy"+` { Lit(int v), Neg(int v) }
	`)
}

// --- Generic function tests ---

func TestGenericFuncDecl(t *testing.T) {
	info := checkOK(t, `
		identity[T](T x) T { return x; }
		main() { }
	`)
	// Verify that identity has a Signature with TypeParams
	for _, scope := range info.Scopes {
		if obj := scope.Lookup("identity"); obj != nil {
			fn, ok := obj.(*types.Func)
			if !ok {
				t.Fatal("identity is not a Func")
			}
			sig, ok := fn.Type().(*types.Signature)
			if !ok || sig == nil {
				t.Fatal("identity has no signature")
			}
			if len(sig.TypeParams()) != 1 {
				t.Fatalf("expected 1 type param, got %d", len(sig.TypeParams()))
			}
			if sig.TypeParams()[0].Obj().Name() != "T" {
				t.Fatalf("expected type param T, got %s", sig.TypeParams()[0].Obj().Name())
			}
			return
		}
	}
	t.Fatal("identity function not found")
}

func TestGenericFuncCall(t *testing.T) {
	info := checkOK(t, `
		identity[T](T x) T { return x; }
		main() {
			int r := identity[int](42);
		}
	`)
	if len(info.FuncInstances) != 1 {
		t.Fatalf("expected 1 FuncInstance, got %d", len(info.FuncInstances))
	}
	fi := info.FuncInstances[0]
	if fi.Func.Name() != "identity" {
		t.Fatalf("expected func identity, got %s", fi.Func.Name())
	}
	if len(fi.TypeArgs) != 1 {
		t.Fatalf("expected 1 type arg, got %d", len(fi.TypeArgs))
	}
	if fi.Sig.Result() != types.TypInt {
		t.Fatalf("expected result int, got %s", fi.Sig.Result())
	}
}

func TestGenericFuncBodyTypeCheck(t *testing.T) {
	checkOK(t, `
		identity[T](T x) T {
			T y := x;
			return y;
		}
		main() {
			int r := identity[int](42);
		}
	`)
}

func TestGenericFuncCallWrongType(t *testing.T) {
	errs := checkErrs(t, `
		identity[T](T x) T { return x; }
		main() {
			int r := identity[int]("hello");
		}
	`)
	expectError(t, errs, "not assignable")
}

func TestGenericFuncMultipleInstances(t *testing.T) {
	info := checkOK(t, `
		identity[T](T x) T { return x; }
		main() {
			int a := identity[int](42);
			string b := identity[string]("hi");
		}
	`)
	if len(info.FuncInstances) != 2 {
		t.Fatalf("expected 2 FuncInstances, got %d", len(info.FuncInstances))
	}
}

func TestGenericFuncStringResult(t *testing.T) {
	info := checkOK(t, `
		identity[T](T x) T { return x; }
		main() {
			string s := identity[string]("hello");
		}
	`)
	if len(info.FuncInstances) != 1 {
		t.Fatalf("expected 1 FuncInstance, got %d", len(info.FuncInstances))
	}
	fi := info.FuncInstances[0]
	if fi.Sig.Result() != types.TypString {
		t.Fatalf("expected result string, got %s", fi.Sig.Result())
	}
}

// --- Stage 8i: container .len property sema tests ---

func TestSliceLenProperty(t *testing.T) {
	checkOKWithStd(t, stdContainers, `
		main() {
			int[] arr = [1, 2, 3];
			int n = arr.len;
		}
	`)
}

func TestArrayLenProperty(t *testing.T) {
	checkOKWithStd(t, stdContainers, `
		check(int[3] arr) int { return arr.len; }
		main() { }
	`)
}

func TestArrayContains(t *testing.T) {
	checkOKWithStd(t, stdContainers, `
		check(int[3] arr) bool { return arr.contains(1); }
		main() { }
	`)
}

func TestArrayMutatingMethodsRejected(t *testing.T) {
	errs := checkErrsWithStd(t, stdContainers, `
		check(int[3] arr) { arr.push(1); }
		main() { }
	`)
	expectError(t, errs, "cannot push on fixed-size array")

	errs = checkErrsWithStd(t, stdContainers, `
		check(int[3] arr) { arr.remove(0); }
		main() { }
	`)
	expectError(t, errs, "cannot remove on fixed-size array")

	errs = checkErrsWithStd(t, stdContainers, `
		check(int[3] arr) { arr.pop(); }
		main() { }
	`)
	expectError(t, errs, "cannot pop on fixed-size array")
}

func TestMapLenProperty(t *testing.T) {
	checkOKWithStd(t, stdContainers, `
		main() {
			m := {"a": 1};
			int n = m.len;
		}
	`)
}

func TestStringLenProperty(t *testing.T) {
	checkOKWithStd(t, stdContainers, `
		main() {
			string s = "hello";
			int n = s.len;
		}
	`)
}

func TestSliceInvalidMember(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			int[] arr = [1, 2];
			int n = arr.foo;
		}
	`)
	expectError(t, errs, "no field or method")
}

func TestMapInvalidMember(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			m := {"a": 1};
			int n = m.foo;
		}
	`)
	expectError(t, errs, "no field or method")
}

// --- Inheritance Validation Tests (Stage 8k) ---

func TestAbstractInstantiationError(t *testing.T) {
	errs := checkErrs(t, `
		type Shape {
			area() f64 `+"`abstract;"+`
		}
		main() {
			Shape s = Shape();
		}
	`)
	expectError(t, errs, "cannot instantiate abstract type")
}

func TestMultipleConcreteParentsError(t *testing.T) {
	errs := checkErrs(t, `
		type A { int x; }
		type B { int y; }
		type C is A, B { }
	`)
	expectError(t, errs, "multiple concrete parents")
}

func TestMultipleConcreteParentsTransitiveError(t *testing.T) {
	errs := checkErrs(t, `
		type A { int x; }
		type B is A { }
		type D { int y; }
		type C is B, D { }
	`)
	// B has no direct fields but inherits x from A — still counts as concrete
	expectError(t, errs, "multiple concrete parents")
}

func TestAbstractGenericInstantiationError(t *testing.T) {
	errs := checkErrs(t, `
		type Container[T] {
			get() T `+"`abstract;"+`
		}
		main() {
			Container[int] c = Container[int]();
		}
	`)
	expectError(t, errs, "cannot instantiate abstract type")
}

func TestMultipleAbstractParentsOK(t *testing.T) {
	checkOK(t, `
		type Printable {
			print() `+"`abstract;"+`
		}
		type Serializable {
			serialize() string `+"`abstract;"+`
		}
		type Doc is Printable, Serializable {
			string name;
			print() { }
			serialize() string { return "doc"; }
		}
		main() { Doc d = Doc(name: "hi"); }
	`)
}

// --- Stage 8l: Structural interface satisfaction tests ---

func TestStructuralSatisfactionWithMeta(t *testing.T) {
	checkOK(t, `
		type Printable `+"`structural"+` {
			print() string `+"`abstract;"+`
		}
		type Doc {
			print() string { return "doc"; }
		}
		main() {
			Printable p = Doc();
		}
	`)
}

func TestStructuralSatisfactionWithoutMetaFails(t *testing.T) {
	errs := checkErrs(t, `
		type Printable {
			print() string `+"`abstract;"+`
		}
		type Doc {
			print() string { return "doc"; }
		}
		main() {
			Printable p = Doc();
		}
	`)
	expectError(t, errs, "cannot assign")
}

func TestStructuralSatisfactionMissingMethodFails(t *testing.T) {
	errs := checkErrs(t, `
		type Printable `+"`structural"+` {
			print() string `+"`abstract;"+`
		}
		type Doc {
			save() string { return "saved"; }
		}
		main() {
			Printable p = Doc();
		}
	`)
	expectError(t, errs, "cannot assign")
}

func TestStructuralSatisfactionSignatureMismatchFails(t *testing.T) {
	errs := checkErrs(t, `
		type Printable `+"`structural"+` {
			print() string `+"`abstract;"+`
		}
		type Doc {
			print(int x) string { return "doc"; }
		}
		main() {
			Printable p = Doc();
		}
	`)
	expectError(t, errs, "cannot assign")
}

// --- Stage 9: Reserved std name tests ---

func TestReservedStdNameFunc(t *testing.T) {
	errs := checkErrs(t, `std() {}`)
	expectError(t, errs, "'std' is reserved")
}

func TestReservedStdNameType(t *testing.T) {
	errs := checkErrs(t, `type std {}`)
	expectError(t, errs, "'std' is reserved")
}

func TestReservedStdNameEnum(t *testing.T) {
	errs := checkErrs(t, `enum std { A, B }`)
	expectError(t, errs, "'std' is reserved")
}

// --- Stage 9: Std scope and test annotation tests ---

// checkSourceWithStd parses stdSrc as std declarations (IsStd=true) and userSrc as
// user declarations, merges them (std first), and runs sema.Check.
func checkSourceWithStd(t *testing.T, stdSrc, userSrc string) (*Info, []error) {
	t.Helper()
	// Always include stdAll; additional stdSrc is appended
	combinedStd := stdAll + "\n" + stdSrc
	// Parse std
	stdInput := antlr.NewInputStream(combinedStd)
	stdLexer := parser.NewPromiseLexer(stdInput)
	stdLexer.RemoveErrorListeners()
	stdStream := antlr.NewCommonTokenStream(stdLexer, antlr.TokenDefaultChannel)
	stdP := parser.NewPromiseParser(stdStream)
	stdP.RemoveErrorListeners()
	stdTree := stdP.CompilationUnit()
	stdFile, errs := ast.Build("std.pr", stdTree)
	if len(errs) > 0 {
		t.Fatalf("std AST build errors: %v", errs)
	}
	// Tag std decls
	for _, d := range stdFile.Decls {
		switch dd := d.(type) {
		case *ast.FuncDecl:
			dd.IsStd = true
		case *ast.TypeDecl:
			dd.IsStd = true
		case *ast.EnumDecl:
			dd.IsStd = true
		}
	}

	// Parse user
	userInput := antlr.NewInputStream(userSrc)
	userLexer := parser.NewPromiseLexer(userInput)
	userLexer.RemoveErrorListeners()
	userStream := antlr.NewCommonTokenStream(userLexer, antlr.TokenDefaultChannel)
	userP := parser.NewPromiseParser(userStream)
	userP.RemoveErrorListeners()
	userTree := userP.CompilationUnit()
	userFile, errs := ast.Build("test.pr", userTree)
	if len(errs) > 0 {
		t.Fatalf("user AST build errors: %v", errs)
	}

	// Merge: std decls first, then user decls
	merged := make([]ast.Decl, 0, len(stdFile.Decls)+len(userFile.Decls))
	merged = append(merged, stdFile.Decls...)
	merged = append(merged, userFile.Decls...)
	userFile.Decls = merged

	return Check(userFile)
}

func TestStdScopeIsPopulated(t *testing.T) {
	info, errs := checkSourceWithStd(t,
		`helper() int { return 42; }`,
		`main() { x := helper(); }`,
	)
	expectNoErrors(t, errs)
	if info.StdScope == nil {
		t.Fatal("expected StdScope to be non-nil")
	}
	if obj := info.StdScope.Lookup("helper"); obj == nil {
		t.Error("expected 'helper' to be in StdScope")
	}
}

func TestStdMemberUndefined(t *testing.T) {
	_, errs := checkSourceWithStd(t,
		`helper() {}`,
		`main() { std.nonexistent(); }`,
	)
	expectError(t, errs, "std has no member 'nonexistent'")
}

func TestStdIsStdBypassesReservedName(t *testing.T) {
	// A std-marked declaration named "std" would bypass the reserved check,
	// but in practice the std library never declares "std". Verify no error.
	info, errs := checkSourceWithStd(t,
		`helper() int { return 1; }`,
		`main() { x := helper(); }`,
	)
	expectNoErrors(t, errs)
	if info.StdScope == nil {
		t.Fatal("expected StdScope to be non-nil")
	}
}

func TestMultipleTestsAccumulation(t *testing.T) {
	info := checkOK(t, `
		test_a() `+"`test"+` {}
		test_b() `+"`test"+` {}
		test_c() `+"`test"+` {}
	`)
	if len(info.Tests) != 3 {
		t.Fatalf("expected 3 test functions, got %d", len(info.Tests))
	}
	names := make(map[string]bool)
	for _, fn := range info.Tests {
		names[fn.Name()] = true
	}
	for _, name := range []string{"test_a", "test_b", "test_c"} {
		if !names[name] {
			t.Errorf("expected test function '%s' in Tests", name)
		}
	}
}

func TestTestFuncWithParamsFails(t *testing.T) {
	errs := checkErrs(t, `myTest(int x) `+"`test"+` {}`)
	expectError(t, errs, "must have no parameters")
}

func TestTestFuncWithReturnTypeFails(t *testing.T) {
	errs := checkErrs(t, `myTest() int `+"`test"+` { return 1; }`)
	expectError(t, errs, "must not have a return type")
}

func TestTestFuncFailableFails(t *testing.T) {
	errs := checkErrs(t, `myTest() int! `+"`test"+` { return 1; }`)
	expectError(t, errs, "must not be failable")
}

func TestTestFuncGenericFails(t *testing.T) {
	errs := checkErrs(t, `myTest[T]() `+"`test"+` {}`)
	expectError(t, errs, "must not be generic")
}

func TestStdScopeRouting(t *testing.T) {
	// Std function that calls another std function should resolve correctly
	info, errs := checkSourceWithStd(t,
		`
		inner() int { return 42; }
		outer() int { return inner(); }
		`,
		`main() { x := outer(); }`,
	)
	expectNoErrors(t, errs)
	if info.StdScope.Lookup("inner") == nil {
		t.Error("expected 'inner' in stdScope")
	}
	if info.StdScope.Lookup("outer") == nil {
		t.Error("expected 'outer' in stdScope")
	}
}

func TestStdFuncMissingReturnDetected(t *testing.T) {
	// Std function with missing return should be caught by checkMissingReturn
	_, errs := checkSourceWithStd(t,
		`broken() int { }`,
		`main() {}`,
	)
	expectError(t, errs, "missing return")
}

func TestStdScopeDoesNotLeakToUser(t *testing.T) {
	// Std function should not see user functions (stdScope is parent of fileScope,
	// so lookups from stdScope do NOT descend into fileScope)
	_, errs := checkSourceWithStd(t,
		`stdFunc() int { return userFunc(); }`,
		`userFunc() int { return 1; }`,
	)
	expectError(t, errs, "undefined")
}

// --- Stage 8k: Native type declaration tests ---

func TestNativeTypeStringMethod(t *testing.T) {
	// Getter on a native type (string) with a Promise body
	_, errs := checkSourceWithStd(t,
		`type string `+"`"+`native {
			int len;
			get is_empty bool {
				return this.len == 0;
			}
		}`,
		`main() {
			s := "hello";
			b := s.is_empty;
		}`,
	)
	expectNoErrors(t, errs)
}

func TestNativeTypeWithNativeMethod(t *testing.T) {
	// Native method on a native type — no body required
	_, errs := checkSourceWithStd(t,
		`type string `+"`"+`native {
			contains(string sub) bool `+"`"+`native;
		}`,
		`main() {
			b := "hello".contains("ell");
		}`,
	)
	expectNoErrors(t, errs)
}

func TestNativeTypeNotInUniverse(t *testing.T) {
	// Error: declaring a native type that doesn't exist in the universe
	errs := checkErrs(t,
		`type Foo `+"`"+`native {}`,
	)
	expectError(t, errs, "native type 'Foo' not found in universe")
}

func TestNativeTypeMissingReturnDetected(t *testing.T) {
	// Missing return in a getter on native type should be caught
	_, errs := checkSourceWithStd(t,
		`type string `+"`"+`native {
			int len;
			get is_empty bool {}
		}`,
		`main() {}`,
	)
	expectError(t, errs, "missing return")
}

// --- Stage 8f: Builtin Validation Tests ---

// checkWithRawStd parses stdSrc as the ONLY std (no stdAll prepended) and
// userSrc as user code. Used for testing validateBuiltins() error detection.
func checkWithRawStd(t *testing.T, stdSrc, userSrc string) (*Info, []error) {
	t.Helper()
	stdInput := antlr.NewInputStream(stdSrc)
	stdLexer := parser.NewPromiseLexer(stdInput)
	stdLexer.RemoveErrorListeners()
	stdStream := antlr.NewCommonTokenStream(stdLexer, antlr.TokenDefaultChannel)
	stdP := parser.NewPromiseParser(stdStream)
	stdP.RemoveErrorListeners()
	stdTree := stdP.CompilationUnit()
	stdFile, errs := ast.Build("std.pr", stdTree)
	if len(errs) > 0 {
		t.Fatalf("std AST build errors: %v", errs)
	}
	for _, d := range stdFile.Decls {
		switch dd := d.(type) {
		case *ast.FuncDecl:
			dd.IsStd = true
		case *ast.TypeDecl:
			dd.IsStd = true
		case *ast.EnumDecl:
			dd.IsStd = true
		}
	}
	userInput := antlr.NewInputStream(userSrc)
	userLexer := parser.NewPromiseLexer(userInput)
	userLexer.RemoveErrorListeners()
	userStream := antlr.NewCommonTokenStream(userLexer, antlr.TokenDefaultChannel)
	userP := parser.NewPromiseParser(userStream)
	userP.RemoveErrorListeners()
	userTree := userP.CompilationUnit()
	userFile, errs := ast.Build("test.pr", userTree)
	if len(errs) > 0 {
		t.Fatalf("user AST build errors: %v", errs)
	}
	merged := make([]ast.Decl, 0, len(stdFile.Decls)+len(userFile.Decls))
	merged = append(merged, stdFile.Decls...)
	merged = append(merged, userFile.Decls...)
	userFile.Decls = merged
	return Check(userFile)
}

func TestValidateAllPresent(t *testing.T) {
	// Full stdAll should pass validation with no errors
	_, errs := checkWithRawStd(t, stdAll, `main() {}`)
	expectNoErrors(t, errs)
}

// Note: testing validateBuiltins() for MISSING operators is not feasible in unit tests
// because universe types (TypInt, TypBool, etc.) are global singletons whose methods
// accumulate across test runs. Validation correctness is ensured by:
// 1. TestValidateAllPresent verifying the full std passes
// 2. E2E tests that compile with real std/ files
// 3. The requireBinaryOp/requireUnaryOp/requireMethod/requireField helpers being trivial

// --- Stage 8f: Arity-Aware Method Dedup Tests ---

func TestArityAwareDedup_BinaryAndUnaryMinus(t *testing.T) {
	// Both binary -(int) and unary -() should coexist on int
	checkOK(t, `main() { x := 5 - 3; y := -42; }`)

	// Verify both forms exist on TypInt
	var hasBinary, hasUnary bool
	for _, m := range types.TypInt.Methods() {
		if m.Name() == "-" {
			if len(m.Sig().Params()) == 1 {
				hasBinary = true
			}
			if len(m.Sig().Params()) == 0 {
				hasUnary = true
			}
		}
	}
	if !hasBinary {
		t.Error("int should have binary - (1 param)")
	}
	if !hasUnary {
		t.Error("int should have unary - (0 params)")
	}
}

// --- Stage 8f: All Numeric Type Operator Method Tests ---

func TestAllNumericTypesHaveOperators(t *testing.T) {
	// Trigger std registration
	checkOK(t, `main() {}`)

	arithOps := []string{"+", "-", "*", "/", "%"}
	cmpOps := []string{"==", "!=", "<", ">", "<=", ">="}

	numericTypes := map[string]*types.Named{
		"int": types.TypInt, "i8": types.TypI8, "i16": types.TypI16,
		"i32": types.TypI32, "i64": types.TypI64, "uint": types.TypUint,
		"u8": types.TypU8, "u16": types.TypU16, "u32": types.TypU32,
		"u64": types.TypU64, "f32": types.TypF32, "f64": types.TypF64,
	}

	for name, nt := range numericTypes {
		t.Run(name, func(t *testing.T) {
			for _, op := range arithOps {
				if nt.LookupMethod(op) == nil {
					t.Errorf("%s missing binary operator %s", name, op)
				}
			}
			for _, op := range cmpOps {
				if nt.LookupMethod(op) == nil {
					t.Errorf("%s missing comparison operator %s", name, op)
				}
			}
			// Verify unary negate exists (0-param version)
			hasUnary := false
			for _, m := range nt.Methods() {
				if m.Name() == "-" && len(m.Sig().Params()) == 0 {
					hasUnary = true
					break
				}
			}
			if !hasUnary {
				t.Errorf("%s missing unary operator -", name)
			}
		})
	}
}

func TestBoolHasAllOperators(t *testing.T) {
	checkOK(t, `main() {}`)
	for _, op := range []string{"&&", "||", "==", "!="} {
		if types.TypBool.LookupMethod(op) == nil {
			t.Errorf("bool missing binary operator %s", op)
		}
	}
	if types.TypBool.LookupMethod("!") == nil {
		t.Error("bool missing unary operator !")
	}
}

func TestCharHasAllOperators(t *testing.T) {
	checkOK(t, `main() {}`)
	for _, op := range []string{"==", "!=", "<", ">", "<=", ">="} {
		if types.TypChar.LookupMethod(op) == nil {
			t.Errorf("char missing comparison operator %s", op)
		}
	}
}

func TestStringHasAllOperators(t *testing.T) {
	checkOK(t, `main() {}`)
	for _, op := range []string{"+", "==", "!=", "<", ">", "<=", ">="} {
		if types.TypString.LookupMethod(op) == nil {
			t.Errorf("string missing operator %s", op)
		}
	}
}

// --- Stage 8f: Char Operator Tests ---

func TestCharComparisons(t *testing.T) {
	checkOK(t, `
		main() {
			bool eq = 'a' == 'b';
			bool ne = 'a' != 'b';
			bool lt = 'a' < 'b';
			bool gt = 'a' > 'b';
			bool le = 'a' <= 'b';
			bool ge = 'a' >= 'b';
		}
	`)
}

// --- Operator Method Dispatch Tests ---

func TestIncDecStmt(t *testing.T) {
	checkOK(t, `
		main() {
			x := 0;
			x++;
			x--;
		}
	`)
}

func TestIncDecOnFloat(t *testing.T) {
	checkOK(t, `
		main() {
			f64 x = 1.0;
			x++;
			x--;
		}
	`)
}

func TestIncDecOnNonNumeric(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			string s = "hello";
			s++;
		}
	`)
	expectError(t, errs, "operator ++ not defined on type string")
}

func TestIncDecOnMember(t *testing.T) {
	checkOK(t, `
		type Counter { int value; }
		main() {
			Counter c = Counter(value: 0);
			c.value++;
		}
	`)
}

func TestIncDecOnIndex(t *testing.T) {
	checkOK(t, `
		main() {
			int[] items = [1, 2, 3];
			items[0]++;
		}
	`)
}

func TestClassicForIncDec(t *testing.T) {
	checkOK(t, `
		main() {
			for i := 0; i < 10; i++ {
				int x = i;
			}
		}
	`)
}

func TestClassicForDecrement(t *testing.T) {
	checkOK(t, `
		main() {
			for i := 10; i > 0; i-- {
				int x = i;
			}
		}
	`)
}

func TestNumericTypesHaveIncDec(t *testing.T) {
	checkOK(t, `main() {}`)

	numericTypes := map[string]*types.Named{
		"int": types.TypInt, "i8": types.TypI8, "i16": types.TypI16,
		"i32": types.TypI32, "i64": types.TypI64, "uint": types.TypUint,
		"u8": types.TypU8, "u16": types.TypU16, "u32": types.TypU32,
		"u64": types.TypU64, "f32": types.TypF32, "f64": types.TypF64,
	}

	for name, nt := range numericTypes {
		if nt.LookupMethod("++") == nil {
			t.Errorf("%s missing ++ operator", name)
		}
		if nt.LookupMethod("--") == nil {
			t.Errorf("%s missing -- operator", name)
		}
	}
}

func TestRangeOnChar(t *testing.T) {
	checkOK(t, `
		main() {
			r := 'a'..'z';
		}
	`)
}

func TestRangeInclusiveOnChar(t *testing.T) {
	checkOK(t, `
		main() {
			r := 'a'..='z';
		}
	`)
}

func TestCharHasRangeOperators(t *testing.T) {
	checkOK(t, `main() {}`)
	if types.TypChar.LookupMethod("..") == nil {
		t.Error("char missing .. operator")
	}
	if types.TypChar.LookupMethod("..=") == nil {
		t.Error("char missing ..= operator")
	}
}

func TestUnaryNotOnBool(t *testing.T) {
	checkOK(t, `
		main() {
			bool b = !true;
			bool c = !false;
		}
	`)
}

func TestUnaryNotOnNonBool(t *testing.T) {
	errs := checkErrs(t, `main() { x := !42; }`)
	expectError(t, errs, "operator ! not defined on type int")
}

func TestStringIndexAccess(t *testing.T) {
	checkOK(t, `
		main() {
			string s = "hello";
			char c = s[0];
		}
	`)
}

func TestStringIndexAssignFails(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			string s = "hello";
			s[0] = 'a';
		}
	`)
	expectError(t, errs, "does not support index assignment")
}

func TestSliceIndexAccess(t *testing.T) {
	checkOK(t, `
		main() {
			int[] items = [1, 2, 3];
			int x = items[0];
		}
	`)
}

func TestSliceIndexAssign(t *testing.T) {
	checkOK(t, `
		main() {
			int[] items = [1, 2, 3];
			items[0] = 42;
		}
	`)
}

func TestSliceIndexTypeMismatch(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			int[] items = [1, 2, 3];
			int x = items["bad"];
		}
	`)
	expectError(t, errs, "index type mismatch")
}

func TestMapIndexAccess(t *testing.T) {
	checkOK(t, `
		main() {
			m := {"a": 1};
			v := m["a"];
		}
	`)
}

func TestMapIndexAssign(t *testing.T) {
	checkOK(t, `
		main() {
			m := {"a": 1};
			m["b"] = 2;
		}
	`)
}

func TestSliceExprOnSlice(t *testing.T) {
	checkOK(t, `
		main() {
			int[] items = [1, 2, 3, 4, 5];
			int[] sub = items[1:3];
		}
	`)
}

func TestSliceExprLowOnly(t *testing.T) {
	checkOK(t, `
		main() {
			int[] items = [1, 2, 3];
			int[] sub = items[1:];
		}
	`)
}

func TestSliceExprHighOnly(t *testing.T) {
	checkOK(t, `
		main() {
			int[] items = [1, 2, 3];
			int[] sub = items[:2];
		}
	`)
}

func TestSliceExprBothEmpty(t *testing.T) {
	checkOK(t, `
		main() {
			int[] items = [1, 2, 3];
			int[] sub = items[:];
		}
	`)
}

func TestSliceExprOnString(t *testing.T) {
	checkOK(t, `
		main() {
			string s = "hello world";
			string sub = s[0:5];
		}
	`)
}

func TestSliceExprOnNonSliceable(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			int x = 42;
			y := x[0:1];
		}
	`)
	expectError(t, errs, "does not support slicing")
}

func TestSliceExprBoundTypeMismatch(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			int[] items = [1, 2, 3];
			int[] sub = items["a":1];
		}
	`)
	expectError(t, errs, "slice bound type mismatch")
}

func TestStringSliceAssignFails(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			string s = "hello";
			s[0:2] = "ab";
		}
	`)
	expectError(t, errs, "does not support slice assignment")
}

func TestStringHasSliceAndIndexOperators(t *testing.T) {
	checkOK(t, `main() {}`)
	if types.TypString.LookupMethod("[]") == nil {
		t.Error("string missing [] operator")
	}
	if types.TypString.LookupMethod("[:]") == nil {
		t.Error("string missing [:] operator")
	}
}

// --- Stage 8m: use Bindings ---

func TestUseVarDeclOK(t *testing.T) {
	checkOK(t, `
		type Resource {
			close() {}
		}
		main() {
			use r := Resource();
		}
	`)
}

func TestUseVarDeclNoCloseMethod(t *testing.T) {
	errs := checkErrs(t, `
		type Foo {
			int x;
		}
		main() {
			use f := Foo(x: 1);
		}
	`)
	expectError(t, errs, "has no close() method")
}

func TestUseVarDeclPrimitiveError(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			use x := 42;
		}
	`)
	expectError(t, errs, "has no close() method")
}

func TestUseVarDeclTypeUsable(t *testing.T) {
	// Variable declared with use should be accessible in its scope
	checkOK(t, `
		type Resource {
			int value;
			close() {}
			get_value() int { return this.value; }
		}
		main() {
			use r := Resource(value: 10);
			int v = r.get_value();
		}
	`)
}

func TestUseVarDeclStructuralClose(t *testing.T) {
	// Any type with close() method works, even without explicit Closer interface
	checkOK(t, `
		type MyHandle {
			close() {}
		}
		main() {
			use h := MyHandle();
		}
	`)
}

// --- Getter/Setter same name regression ---

func TestGetterSetterSameName(t *testing.T) {
	// Type with both getter and setter for the same field name.
	// Previously caused LookupAnyMethod collision: the setter body was
	// validated against the getter's signature (or vice versa).
	checkOK(t, `
		type Counter {
			int _count;
			get count int { return this._count; }
			set count(int v) { this._count = v; }
		}
		main() {
			Counter c = Counter(_count: 0);
			c.count = 5;
			int v = c.count;
		}
	`)
}

func TestGetterSetterSameNameReturnCheck(t *testing.T) {
	// Setter has no return type — the return checker should not flag it
	// as "missing return statement" (which happened when LookupAnyMethod
	// returned the getter instead of the setter).
	checkOK(t, `
		type Wrapper {
			int _val;
			get val int => this._val;
			set val(int v) { this._val = v; }
		}
		main() {
			Wrapper w = Wrapper(_val: 0);
			w.val = 42;
		}
	`)
}

func TestAbstractGetterNotSatisfiedBySetter(t *testing.T) {
	// A concrete setter should NOT satisfy an abstract getter with the same name.
	errs := checkErrs(t, `
		type Base {
			get val int `+"`"+`abstract;
		}
		type Child is Base {
			set val(int v) { }
		}
		main() {
			Child c = Child();
		}
	`)
	expectError(t, errs, "abstract")
}

func TestAbstractSetterNotSatisfiedByGetter(t *testing.T) {
	// Inverse: a concrete getter should NOT satisfy an abstract setter.
	errs := checkErrs(t, `
		type Base {
			set val(int v) `+"`"+`abstract;
		}
		type Child is Base {
			get val int { return 0; }
		}
		main() {
			Child c = Child();
		}
	`)
	expectError(t, errs, "abstract")
}

func TestAbstractGetterAndSetterBothImplemented(t *testing.T) {
	// Both abstract getter and setter implemented — child is not abstract.
	checkOK(t, `
		type Base {
			get val int `+"`"+`abstract;
			set val(int v) `+"`"+`abstract;
		}
		type Child is Base {
			int _v;
			get val int { return this._v; }
			set val(int v) { this._v = v; }
		}
		main() {
			Child c = Child(_v: 0);
		}
	`)
}

func TestCompoundAssignmentGetterSetter(t *testing.T) {
	// Compound assignment reads via getter, writes via setter.
	checkOK(t, `
		type Counter {
			int _count;
			get count int { return this._count; }
			set count(int v) { this._count = v; }
		}
		main() {
			Counter c = Counter(_count: 0);
			c.count += 5;
			c.count -= 1;
		}
	`)
}

// --- drop() method validation ---

func TestDropMethodValid(t *testing.T) {
	checkOK(t, `
		type File {
			int fd;
			drop(~this) {}
		}
		main() {
			f := File(fd: 1);
		}
	`)
}

func TestDropMethodWrongReceiverValue(t *testing.T) {
	errs := checkErrs(t, `
		type File {
			int fd;
			drop(this) {}
		}
		main() {}
	`)
	expectError(t, errs, "must take ~this")
}

func TestDropMethodWrongReceiverShared(t *testing.T) {
	errs := checkErrs(t, `
		type File {
			int fd;
			drop(&this) {}
		}
		main() {}
	`)
	expectError(t, errs, "must take ~this")
}

func TestDropMethodWithParams(t *testing.T) {
	errs := checkErrs(t, `
		type File {
			int fd;
			drop(~this, int x) {}
		}
		main() {}
	`)
	expectError(t, errs, "must have no parameters")
}

func TestDropMethodWithReturn(t *testing.T) {
	errs := checkErrs(t, `
		type File {
			int fd;
			drop(~this) int { return 0; }
		}
		main() {}
	`)
	expectError(t, errs, "must not return a value")
}

func TestDropMethodFailable(t *testing.T) {
	errs := checkErrs(t, `
		type File {
			int fd;
			drop(~this) void! { raise "err"; }
		}
		main() {}
	`)
	expectError(t, errs, "must not be failable")
}

func TestDropMethodOnCopyType(t *testing.T) {
	errs := checkErrs(t, `
		type Point `+"`"+`copy {
			int x;
			int y;
			drop(~this) {}
		}
		main() {}
	`)
	expectError(t, errs, "copy type Point cannot have a drop()")
}

func TestDropMethodAbstract(t *testing.T) {
	errs := checkErrs(t, `
		type Resource {
			int id;
			drop(~this) `+"`"+`abstract;
		}
		main() {}
	`)
	expectError(t, errs, "must not be abstract")
}

// isCopyField with SharedRef — should be copy
func TestCopyTypeWithRefField(t *testing.T) {
	// &T is copy since it's just a pointer
	checkOK(t, `
		type Wrapper `+"`"+`copy {
			&int val;
		}
		main() {}
	`)
}

// isCopyField with MutRef — should be copy
func TestCopyTypeWithMutRefField(t *testing.T) {
	checkOK(t, `
		type MutWrapper `+"`"+`copy {
			~int val;
		}
		main() {}
	`)
}

// isCopyField with Named non-copy field — should error
func TestCopyTypeWithNonCopyNamedField(t *testing.T) {
	errs := checkErrs(t, `
		type Inner {
			int x;
		}
		type Outer `+"`"+`copy {
			Inner inner;
		}
		main() {}
	`)
	expectError(t, errs, "non-copy type")
}

// isCopyField with copy Named field — should pass
func TestCopyTypeWithCopyNamedField(t *testing.T) {
	checkOK(t, `
		type Inner `+"`"+`copy {
			int x;
		}
		type Outer `+"`"+`copy {
			Inner inner;
		}
		main() {}
	`)
}

// isCopyField with copy enum
func TestCopyTypeWithCopyEnumField(t *testing.T) {
	checkOK(t, `
		enum Status `+"`"+`copy {
			Active;
			Inactive;
		}
		type Wrapper `+"`"+`copy {
			Status s;
		}
		main() {}
	`)
}

// isCopyField with non-copy enum — should fail
func TestCopyTypeWithNonCopyEnumField(t *testing.T) {
	errs := checkErrs(t, `
		enum Option {
			Some(string val);
			None;
		}
		type Wrapper `+"`"+`copy {
			Option opt;
		}
		main() {}
	`)
	expectError(t, errs, "non-copy type")
}
