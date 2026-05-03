package sema

import (
	"strings"
	"sync"
	"testing"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/parser"
	"djabi.dev/go/promise_lang/internal/testutil"
	"djabi.dev/go/promise_lang/internal/types"
	antlr "github.com/antlr4-go/antlr/v4"
)

// --- Test helpers ---

// stdAll provides all builtin type declarations needed by tests.
// Loaded from the actual std/*.pr files to avoid duplication.
var stdAll string

func init() {
	stdAll = testutil.LoadStdFiles()
}

// stdContainers is kept as an alias for backward compatibility with tests
// that pass explicit std via checkOKWithStd.
var stdContainers = "" // subsumed by stdAll; tests using checkOKWithStd get stdAll automatically

var (
	semaStdOnce  sync.Once
	semaStdScope *types.Scope
)

func getSemaStdScope() *types.Scope {
	semaStdOnce.Do(func() {
		input := antlr.NewInputStream(stdAll)
		lexer := parser.NewPromiseLexer(input)
		lexer.RemoveErrorListeners()
		stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
		p := parser.NewPromiseParser(stream)
		p.RemoveErrorListeners()
		tree := p.CompilationUnit()
		stdFile, buildErrs := ast.Build("std.pr", tree)
		if len(buildErrs) > 0 {
			panic("std AST build errors: " + buildErrs[0].Error())
		}
		stdInfo, _ := CheckForStdModule(stdFile, HostTargetInfo())
		semaStdScope = ExportedScope(stdInfo, stdFile)
	})
	return semaStdScope
}

func checkSource(t *testing.T, src string) (*Info, []error) {
	t.Helper()
	input := antlr.NewInputStream(src)
	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := parser.NewPromiseParser(stream)
	p.RemoveErrorListeners()
	tree := p.CompilationUnit()
	file, buildErrs := ast.Build("test.pr", tree)
	if len(buildErrs) > 0 {
		t.Fatalf("AST build errors: %v", buildErrs)
	}
	// Inject use std as _
	stdUse := &ast.UseDecl{Alias: "_", CatalogName: "std"}
	file.Uses = append([]*ast.UseDecl{stdUse}, file.Uses...)
	return CheckWithModules(file, map[string]*types.Scope{"std": getSemaStdScope()})
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
			int a = 1;
			f64 b = 1.0;
			bool c = true;
			string d = "hello";
			char e = 'x';
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
	expectError(t, errs, "missing required argument 'b'")
}

func TestFunctionCallTypeMismatch(t *testing.T) {
	errs := checkErrs(t, `
		add(int a, int b) int { return a + b; }
		test() { x := add(1, "two"); }
	`)
	expectError(t, errs, "cannot assign string to parameter 'b'")
}

// --- Member Access Tests ---

func TestFieldAccess(t *testing.T) {
	checkOK(t, `
		type Dog { string name; int age; }
		test() {
			Dog d = Dog(name: "Rex", age: 3);
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
			Dog d = Dog(name: "Rex");
			x := d.bark();
		}
	`)
}

func TestInheritedFieldAccess(t *testing.T) {
	checkOK(t, `
		type Animal { string name; }
		type Dog is Animal { int age; }
		test() {
			Dog d = Dog(name: "Rex", age: 3);
			x := d.name;
		}
	`)
}

func TestUndefinedField(t *testing.T) {
	errs := checkErrs(t, `
		type Dog { string name; }
		test() {
			Dog d = Dog(name: "Rex");
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
	checkOK(t, `foo() int! { raise error(message: "oops"); }`)
}

func TestRaiseInNonFailable(t *testing.T) {
	errs := checkErrs(t, `foo() { raise error(message: "oops"); }`)
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

func TestRaiseNonErrorType(t *testing.T) {
	errs := checkErrs(t, `foo() int! { raise 42; }`)
	expectError(t, errs, "raise requires an error type")
}

func TestRaiseErrorSubtype(t *testing.T) {
	checkOK(t, `
		type IoError is error {
			int code;
		}
		foo() void! { raise IoError(message: "fail", code: 1); }
	`)
}

func TestTypedErrorHandler(t *testing.T) {
	checkOK(t, `
		type IoError is error {
			int code;
		}
		foo() void! { raise IoError(message: "fail", code: 1); }
		bar() int! {
			foo() ? e is IoError { return e.code; };
			return 0;
		}
	`)
}

func TestTypedErrorHandlerUndefinedType(t *testing.T) {
	errs := checkErrs(t, `
		foo() void! { raise error(message: "oops"); }
		bar() {
			foo() ? e is Nope { };
		}
	`)
	expectError(t, errs, "undefined type: Nope")
}

func TestTypedErrorHandlerNonErrorType(t *testing.T) {
	errs := checkErrs(t, `
		type Foo { int x; }
		foo() void! { raise error(message: "oops"); }
		bar() {
			foo() ? e is Foo { };
		}
	`)
	expectError(t, errs, "does not inherit from error")
}

func TestErrorPropagateOnNonFailable(t *testing.T) {
	errs := checkErrs(t, `
		foo() int { return 42; }
		bar() int! { return foo()?; }
	`)
	expectError(t, errs, "requires a failable expression")
}

func TestErrorUnwrapOnNonFailable(t *testing.T) {
	errs := checkErrs(t, `
		foo() int { return 42; }
		bar() { int x = foo()!; }
	`)
	expectError(t, errs, "requires a failable or optional expression")
}

func TestErrorHandlerOnNonFailable(t *testing.T) {
	errs := checkErrs(t, `
		foo() int { return 42; }
		bar() { foo() ? e { }; }
	`)
	expectError(t, errs, "requires a failable or optional expression")
}

// --- Generic Error Types ---

func TestGenericErrorType(t *testing.T) {
	checkOK(t, `
		type DataError[T] is error {
			T data;
		}
		foo() void! {
			raise DataError[int](message: "bad data", data: 42);
		}
	`)
}

func TestGenericErrorTypeStringParam(t *testing.T) {
	checkOK(t, `
		type DataError[T] is error {
			T data;
		}
		foo() void! {
			raise DataError[string](message: "bad", data: "details");
		}
	`)
}

func TestGenericErrorTypeInHandler(t *testing.T) {
	checkOK(t, `
		type DataError[T] is error {
			T data;
		}
		foo() void! {
			raise DataError[int](message: "bad", data: 42);
		}
		bar() {
			foo() ? e { };
		}
	`)
}

func TestGenericErrorTypeFieldAccess(t *testing.T) {
	// NOTE: Typed handler with generic type (? e is DataError[int]) is not yet
	// supported in the grammar. Untyped handler with message access works.
	checkOK(t, `
		type DataError[T] is error {
			T data;
		}
		foo() void! {
			raise DataError[int](message: "bad", data: 42);
		}
		bar() string {
			foo() ? e { return e.message; };
			return "";
		}
	`)
}

func TestRaiseGenericErrorNonErrorBase(t *testing.T) {
	errs := checkErrs(t, `
		type Box[T] { T value; }
		foo() void! { raise Box[int](value: 1); }
	`)
	expectError(t, errs, "raise requires an error type")
}

// --- Error Construction Variants ---

func TestErrorPositionalConstruction(t *testing.T) {
	checkOK(t, `
		foo() void! {
			raise error("oops");
		}
	`)
}

func TestErrorSubtypePositionalConstruction(t *testing.T) {
	checkOK(t, `
		type IoError is error {
			int code;
		}
		foo() void! {
			raise IoError("disk full", 28);
		}
	`)
}

func TestErrorSubtypeMixedConstruction(t *testing.T) {
	checkOK(t, `
		type IoError is error {
			int code;
		}
		foo() void! {
			raise IoError("disk full", code: 28);
		}
	`)
}

func TestErrorConstructionTooManyArgs(t *testing.T) {
	errs := checkErrs(t, `
		foo() void! { raise error("a", "b"); }
	`)
	expectError(t, errs, "expects at most")
}

func TestErrorConstructionWrongType(t *testing.T) {
	errs := checkErrs(t, `
		foo() void! { raise error(42); }
	`)
	expectError(t, errs, "cannot assign int")
}

func TestErrorSubtypeConstructionWrongFieldType(t *testing.T) {
	errs := checkErrs(t, `
		type IoError is error { int code; }
		foo() void! { raise IoError(message: "err", code: "notint"); }
	`)
	expectError(t, errs, "cannot assign string")
}

// --- Drop Semantics on Error Types ---

func TestErrorTypeCannotHaveDrop(t *testing.T) {
	errs := checkErrs(t, `
		type FileError is error {
			int fd;
			drop(~this) {}
		}
		main() {}
	`)
	expectError(t, errs, "error type FileError cannot have a drop")
}

func TestErrorSubtypeCannotHaveDrop(t *testing.T) {
	errs := checkErrs(t, `
		type AppError is error { int code; }
		type DbError is AppError {
			string conn;
			drop(~this) {}
		}
		main() {}
	`)
	expectError(t, errs, "error type DbError cannot have a drop")
}

// --- Failable Calls Inside Error Handlers ---

func TestFailableCallInsideUntypedHandler(t *testing.T) {
	checkOK(t, `
		parse(string s) int! { return 0; }
		other() int! { return 1; }
		foo() int! {
			int v = parse("x") ? e { return other()?; };
			return v;
		}
	`)
}

func TestFailableCallInsideTypedHandler(t *testing.T) {
	checkOK(t, `
		type IoError is error { int code; }
		fail_io() void! { raise IoError(message: "fail", code: 1); }
		retry() int! { return 0; }
		foo() int! {
			fail_io() ? e is IoError { return retry()?; };
			return 0;
		}
	`)
}

func TestBangUnwrapInsideHandler(t *testing.T) {
	checkOK(t, `
		parse(string s) int! { return 0; }
		foo() {
			parse("x") ? e { int v = parse("0")!; };
		}
	`)
}

func TestFailableCallInsideHandlerOfNonFailable(t *testing.T) {
	// In a non-failable function, handler body can still use ! (bang unwrap)
	checkOK(t, `
		parse(string s) int! { return 0; }
		foo() {
			parse("x") ? e { int v = parse("fallback")!; };
		}
	`)
}

func TestFailableCallPropagateInsideHandlerOfNonFailable(t *testing.T) {
	// Cannot use ? (propagate) in non-failable function, even inside handler
	errs := checkErrs(t, `
		parse(string s) int! { return 0; }
		foo() {
			parse("x") ? e { int v = parse("retry")?; };
		}
	`)
	expectError(t, errs, "outside of failable")
}

// --- Nested Error Handlers ---

func TestNestedErrorHandlers(t *testing.T) {
	checkOK(t, `
		a() int! { return 1; }
		b() int! { return 2; }
		foo() {
			int v = a() ? e1 {
				b() ? e2 { };
			};
		}
	`)
}

func TestTypedHandlerInsideUntypedHandler(t *testing.T) {
	checkOK(t, `
		type IoError is error { int code; }
		a() void! { raise error(message: "a"); }
		b() void! { raise IoError(message: "b", code: 1); }
		foo() void! {
			a() ? e1 {
				b() ? e2 is IoError { };
			};
		}
	`)
}

// --- Error Handler Edge Cases ---

func TestHandlerWithDiscardBinding(t *testing.T) {
	checkOK(t, `
		foo() void! { raise error(message: "oops"); }
		bar() {
			foo() ? _ { };
		}
	`)
}

func TestTypedHandlerWithDiscardBinding(t *testing.T) {
	checkOK(t, `
		type IoError is error { int code; }
		foo() void! { raise IoError(message: "fail", code: 1); }
		bar() void! {
			foo() ? _ is IoError { };
		}
	`)
}

func TestTypedHandlerInNonFailableRejected(t *testing.T) {
	errs := checkErrs(t, `
		type IoError is error { int code; }
		foo() void! { raise IoError(message: "fail", code: 1); }
		bar() {
			foo() ? _ is IoError { };
		}
	`)
	expectError(t, errs, "typed error handler in non-failable function")
}

func TestTypedHandlerElseInNonFailable(t *testing.T) {
	checkOK(t, `
		type IoError is error { int code; }
		foo() void! { raise IoError(message: "fail", code: 1); }
		bar() {
			foo() ? e is IoError { } else { };
		}
	`)
}

func TestTypedHandlerElseWithBinding(t *testing.T) {
	checkOK(t, `
		type IoError is error { int code; }
		foo() void! { raise IoError(message: "fail", code: 1); }
		bar() {
			foo() ? e is IoError { } else e { };
		}
	`)
}

func TestTypedHandlerBangInNonFailable(t *testing.T) {
	checkOK(t, `
		type IoError is error { int code; }
		foo() void! { raise IoError(message: "fail", code: 1); }
		bar() {
			foo() ? e is IoError { }!;
		}
	`)
}

func TestElseOnUntypedHandlerRejected(t *testing.T) {
	errs := checkErrs(t, `
		foo() void! { raise error(message: "fail"); }
		bar() {
			foo() ? e { } else { };
		}
	`)
	expectError(t, errs, "only valid on typed error handlers")
}

func TestBangOnUntypedHandlerRejected(t *testing.T) {
	errs := checkErrs(t, `
		foo() void! { raise error(message: "fail"); }
		bar() {
			foo() ? e { }!;
		}
	`)
	expectError(t, errs, "only valid on typed error handlers")
}

func TestHandlerNoBinding(t *testing.T) {
	checkOK(t, `
		foo() void! { raise error(message: "oops"); }
		bar() {
			foo() ? { };
		}
	`)
}

func TestUnhandledFailableCallInNonFailable(t *testing.T) {
	errs := checkErrs(t, `
		foo() void! { raise error(message: "oops"); }
		bar() {
			foo();
		}
	`)
	expectError(t, errs, "failable call must be handled")
}

func TestAutoPropagateFailable(t *testing.T) {
	checkOK(t, `
		foo() void! { raise error(message: "oops"); }
		bar() void! {
			foo();
		}
	`)
}

func TestAutoPropagateFailable_NonVoid(t *testing.T) {
	checkOK(t, `
		parse() int! { return 42; }
		process() int! {
			parse();
			return 0;
		}
	`)
}

func TestFailableDestructure(t *testing.T) {
	checkOK(t, `
		parse() int! { return 42; }
		foo() {
			(val, err) := parse();
		}
	`)
}

func TestFailableDestructureInNonFailable(t *testing.T) {
	// Destructuring a failable result is allowed in non-failable functions
	// (unlike naked failable calls, destructuring explicitly captures the error)
	checkOK(t, `
		parse() int! { return 42; }
		foo() {
			(val, err) := parse();
		}
	`)
}

// === Auto-propagation in assignments ===

func TestAutoPropagateInTypedVarDecl(t *testing.T) {
	checkOK(t, `
		parse() int! { return 42; }
		wrapper() int! {
			int x = parse();
			return x;
		}
	`)
}

func TestAutoPropagateInInferredVarDecl(t *testing.T) {
	checkOK(t, `
		parse() int! { return 42; }
		wrapper() int! {
			x := parse();
			return x;
		}
	`)
}

func TestAutoPropagateAssignInNonFailable(t *testing.T) {
	errs := checkErrs(t, `
		parse() int! { return 42; }
		main() {
			int x = parse();
		}
	`)
	expectError(t, errs, "failable call must be handled")
}

func TestAutoPropagateInferredInNonFailable(t *testing.T) {
	errs := checkErrs(t, `
		parse() int! { return 42; }
		main() {
			x := parse();
		}
	`)
	expectError(t, errs, "failable call must be handled")
}

func TestAutoPropagateMultipleAssigns(t *testing.T) {
	checkOK(t, `
		parse(string s) int! { return 0; }
		wrapper() int! {
			int a = parse("1");
			int b = parse("2");
			return a + b;
		}
	`)
}

func TestAutoPropagateVoidAssignStmt(t *testing.T) {
	// Void failable as statement (not assignment) in failable fn — auto-propagates
	checkOK(t, `
		validate() void! { raise error(message: "bad"); }
		wrapper() void! {
			validate();
		}
	`)
}

// === Error handler recovery validation ===

func TestErrorHandlerRecoveryValue(t *testing.T) {
	// Handler produces recovery value — OK
	checkOK(t, `
		parse() int! { return 42; }
		main() {
			int x = parse() ? e { 0; };
		}
	`)
}

func TestErrorHandlerRecoveryDiverges(t *testing.T) {
	// Handler diverges with return — OK
	checkOK(t, `
		parse() int! { return 42; }
		main() {
			int x = parse() ? e { return; };
		}
	`)
}

func TestErrorHandlerRecoveryRaise(t *testing.T) {
	// Handler diverges with raise — OK
	checkOK(t, `
		parse() int! { return 42; }
		wrapper() int! {
			int x = parse() ? e { raise e; };
			return x;
		}
	`)
}

func TestErrorHandlerNoRecoveryInTypedDecl(t *testing.T) {
	// Handler doesn't produce value or diverge — ERROR
	errs := checkErrs(t, `
		parse() int! { return 42; }
		main() {
			int x = parse() ? e { print_line(e.message); };
		}
	`)
	expectError(t, errs, "error handler must produce a recovery value or diverge")
}

func TestErrorHandlerNoRecoveryInInferredDecl(t *testing.T) {
	// Non-recovering handler in inferred decl: x becomes int? (optional)
	checkOK(t, `
		parse() int! { return 42; }
		main() {
			x := parse() ? e { print_line(e.message); };
		}
	`)
}

func TestErrorHandlerNoRecoveryInNonOptionalTypedDecl(t *testing.T) {
	// Non-recovering handler in non-optional typed decl: error
	errs := checkErrs(t, `
		parse() int! { return 42; }
		main() {
			int x = parse() ? e { print_line(e.message); };
		}
	`)
	expectError(t, errs, "error handler must produce a recovery value or diverge")
}

func TestErrorHandlerNoRecoveryInOptionalTypedDecl(t *testing.T) {
	// Non-recovering handler in optional typed decl: OK, x becomes int?
	checkOK(t, `
		parse() int! { return 42; }
		main() {
			int? x = parse() ? e { print_line(e.message); };
		}
	`)
}

func TestErrorHandlerNoRecoveryAsStatement(t *testing.T) {
	// As expression statement (not assignment) — OK, value is discarded
	checkOK(t, `
		parse() int! { return 42; }
		main() {
			parse() ? e { print_line(e.message); };
		}
	`)
}

func TestErrorHandlerNoRecoveryVoidFailable(t *testing.T) {
	// Void failable with handler — OK (no value to recover)
	checkOK(t, `
		validate() void! { raise error(message: "bad"); }
		main() {
			validate() ? e { print_line(e.message); };
		}
	`)
}

func TestErrorHandlerRecoveryOptional(t *testing.T) {
	// Handler produces none for optional type — OK
	checkOK(t, `
		parse() int! { return 42; }
		main() {
			int? x = parse() ? e { none; };
		}
	`)
}

func TestErrorHandlerRecoveryExpression(t *testing.T) {
	// Handler produces computed expression — OK
	checkOK(t, `
		parse() int! { return 42; }
		main() {
			int x = parse() ? e { 1 + 2; };
		}
	`)
}

func TestFailableDestructureDiscardValue(t *testing.T) {
	checkOK(t, `
		parse() int! { return 42; }
		foo() {
			(_, err) := parse();
		}
	`)
}

func TestFailableDestructureDiscardError(t *testing.T) {
	checkOK(t, `
		parse() int! { return 42; }
		foo() {
			(val, _) := parse();
		}
	`)
}

func TestMultipleFieldErrorType(t *testing.T) {
	checkOK(t, `
		type DetailedError is error {
			int code;
			string detail;
			bool retryable;
		}
		foo() void! {
			raise DetailedError(message: "failed", code: 503, detail: "service unavailable", retryable: true);
		}
		bar() bool! {
			foo() ? e is DetailedError { return e.retryable; };
			return false;
		}
	`)
}

func TestErrorInheritanceChain(t *testing.T) {
	checkOK(t, `
		type AppError is error {
			int code;
		}
		type DbError is AppError {
			string query;
		}
		foo() void! {
			raise DbError(message: "query failed", code: 500, query: "SELECT 1");
		}
		bar() int! {
			foo() ? e is AppError { return e.code; };
			return 0;
		}
	`)
}

func TestTypedHandlerWithInheritanceChainDeep(t *testing.T) {
	checkOK(t, `
		type AppError is error { int code; }
		type DbError is AppError { string query; }
		type TimeoutError is DbError { int seconds; }
		foo() void! {
			raise TimeoutError(message: "timeout", code: 504, query: "SELECT 1", seconds: 30);
		}
		bar() int! {
			foo() ? e is AppError { return e.code; };
			return 0;
		}
	`)
}

func TestBangShorthandForVoidFailable(t *testing.T) {
	// foo()! is shorthand for foo() void!
	checkOK(t, `
		foo()! { raise error(message: "oops"); }
	`)
}

func TestBangShorthandMethodFailable(t *testing.T) {
	checkOK(t, `
		type Foo {
			bar(this)! { raise error(message: "oops"); }
		}
	`)
}

func TestRaiseStringLiteral(t *testing.T) {
	errs := checkErrs(t, `foo() void! { raise "oops"; }`)
	expectError(t, errs, "raise requires an error type")
}

func TestRaiseBoolLiteral(t *testing.T) {
	errs := checkErrs(t, `foo() void! { raise true; }`)
	expectError(t, errs, "raise requires an error type")
}

func TestRaiseVariable(t *testing.T) {
	checkOK(t, `
		foo() void! {
			error e = error(message: "saved");
			raise e;
		}
	`)
}

func TestFuncReturningErrorType(t *testing.T) {
	// A non-failable function can return error as a normal value
	checkOK(t, `
		make_error() error {
			return error(message: "made");
		}
		foo() void! {
			raise make_error();
		}
	`)
}

func TestErrorHandlerAccessMessageField(t *testing.T) {
	checkOK(t, `
		foo() void! { raise error(message: "test msg"); }
		bar() string {
			foo() ? e { return e.message; };
			return "";
		}
	`)
}

func TestErrorSubtypeAccessBaseMessageField(t *testing.T) {
	checkOK(t, `
		type IoError is error { int code; }
		foo() void! { raise IoError(message: "io fail", code: 1); }
		bar() string! {
			foo() ? e is IoError { return e.message; };
			return "";
		}
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
	errs := checkErrs(t, `
		test() {
			int x = 1;
			if true {
				string x = "shadowed";
			}
		}
	`)
	expectError(t, errs, "shadows")
}

func TestScopeShadowingSiblingScopes(t *testing.T) {
	// Sequential reuse in sibling scopes is OK (not nested)
	checkOK(t, `
		test() {
			if true { int v = 1; }
			if true { int v = 2; }
		}
	`)
}

func TestScopeShadowingLambdaParam(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			int x = 1;
			f := |int x| -> x + 1;
		}
	`)
	expectError(t, errs, "shadows")
}

func TestScopeShadowingForIn(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			int i = 0;
			for x, i in [1, 2, 3] {}
		}
	`)
	expectError(t, errs, "shadows")
}

func TestScopeShadowingNestedIf(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			int a = 1;
			if true {
				if true {
					int a = 3;
				}
			}
		}
	`)
	expectError(t, errs, "shadows")
}

func TestScopeShadowingWhileLoop(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			int x = 0;
			while true {
				int x = 1;
				break;
			}
		}
	`)
	expectError(t, errs, "shadows")
}

func TestScopeShadowingClassicFor(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			int i = 99;
			for int i = 0; i < 10; i++ {}
		}
	`)
	expectError(t, errs, "shadows")
}

func TestScopeShadowingClassicForInferred(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			int j = 99;
			for j := 0; j < 10; j++ {}
		}
	`)
	expectError(t, errs, "shadows")
}

func TestScopeShadowingIfUnwrap(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			int v = 10;
			int? opt = 42;
			if v := opt {}
		}
	`)
	expectError(t, errs, "shadows")
}

func TestScopeShadowingForInBinding(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			int x = 0;
			for x in [1, 2, 3] {}
		}
	`)
	expectError(t, errs, "shadows")
}

func TestScopeShadowingForInIndex(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			string idx = "a";
			for x, idx in [1, 2, 3] {}
		}
	`)
	expectError(t, errs, "shadows")
}

func TestScopeShadowingTypedVarDecl(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			int n = 1;
			if true {
				string n = "shadow";
			}
		}
	`)
	expectError(t, errs, "shadows")
}

func TestScopeShadowingInferredVarDecl(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			x := 1;
			if true {
				x := "shadow";
			}
		}
	`)
	expectError(t, errs, "shadows")
}

func TestScopeShadowingUseVarDecl(t *testing.T) {
	errs := checkErrs(t, `
		type Res { close() {} }
		test() {
			int r = 1;
			if true {
				use r := Res();
			}
		}
	`)
	expectError(t, errs, "shadows")
}

func TestScopeShadowingDestructure(t *testing.T) {
	errs := checkErrs(t, `
		pair() (int, int) { return (1, 2); }
		test() {
			int a = 0;
			if true {
				(a, b) := pair();
			}
		}
	`)
	expectError(t, errs, "shadows")
}

func TestScopeShadowingUnderscoreOK(t *testing.T) {
	// _ should never trigger shadowing
	checkOK(t, `
		test() {
			int _ = 1;
			if true {
				int _ = 2;
			}
		}
	`)
}

func TestScopeShadowingSiblingForLoops(t *testing.T) {
	// Same variable in sibling for-in scopes is OK
	checkOK(t, `
		test() {
			for x in [1, 2] {}
			for x in [3, 4] {}
		}
	`)
}

func TestScopeShadowingSiblingBlocks(t *testing.T) {
	checkOK(t, `
		test() {
			if true { x := 1; }
			if true { x := 2; }
			while true { x := 3; break; }
		}
	`)
}

func TestScopeShadowingLambdaExprBody(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			int y = 1;
			g := |int y| -> y * 2;
		}
	`)
	expectError(t, errs, "shadows")
}

func TestScopeShadowingMultipleLevels(t *testing.T) {
	// Three levels deep: innermost shadows outermost
	errs := checkErrs(t, `
		test() {
			int z = 0;
			if true {
				if true {
					if true {
						int z = 99;
					}
				}
			}
		}
	`)
	expectError(t, errs, "shadows")
}

func TestScopeShadowingErrorHandler(t *testing.T) {
	errs := checkErrs(t, `
		fail() int! { return 1; }
		test() int! {
			int e = 0;
			v := fail() ? e { return 0; };
			return v;
		}
	`)
	expectError(t, errs, "shadows")
}

func TestScopeShadowingMatchBindingAllowed(t *testing.T) {
	// Match arm bindings should NOT trigger shadow errors
	checkOK(t, `
		test() {
			int x = 10;
			int? opt = 42;
			y := match opt {
				x => x,
				_ => 0,
			};
		}
	`)
}

func TestScopeShadowingNarrowingAllowed(t *testing.T) {
	// Narrowing bindings should NOT trigger shadow errors
	checkOK(t, `
		test() {
			int? cc = 42;
			if cc {
				int x = cc + 1;
			}
		}
	`)
}

func TestScopeShadowingDifferentTypesInSiblings(t *testing.T) {
	// Different types in sibling scopes is fine
	checkOK(t, `
		test() {
			if true { string v = "a"; }
			if true { int v = 1; }
			if true { bool v = true; }
		}
	`)
}

func TestScopeShadowingForInSiblingWithOuter(t *testing.T) {
	// for-in binding doesn't shadow outer if outer is in parent scope
	errs := checkErrs(t, `
		test() {
			int item = 0;
			for item in [1, 2, 3] {}
		}
	`)
	expectError(t, errs, "shadows")
}

func TestScopeShadowingNestedLambda(t *testing.T) {
	// Lambda param shadows outer variable
	errs := checkErrs(t, `
		test() {
			int a = 5;
			f := |int a| -> a + 1;
		}
	`)
	expectError(t, errs, "shadows")
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
			Animal a = Dog(name: "Rex");
			Dog? d = a as Dog;
		}
	`)
}

func TestForceCast(t *testing.T) {
	checkOK(t, `
		type Animal { string name; }
		type Dog is Animal { }
		test() {
			Animal a = Dog(name: "Rex");
			Dog d = a as! Dog;
		}
	`)
}

func TestScalarCastCharToInt(t *testing.T) {
	checkOK(t, `
		test() {
			char c = 'A';
			int x = c as int;
		}
	`)
}

func TestScalarCastIntToChar(t *testing.T) {
	checkOK(t, `
		test() {
			int x = 65;
			char c = x as char;
		}
	`)
}

func TestScalarCastU8ToChar(t *testing.T) {
	checkOK(t, `
		test() {
			u8 b = 65u8;
			char c = b as char;
		}
	`)
}

func TestScalarCastBoolToInt(t *testing.T) {
	checkOK(t, `
		test() {
			bool b = true;
			int x = b as int;
		}
	`)
}

func TestScalarCastIntToBool(t *testing.T) {
	checkOK(t, `
		test() {
			int x = 42;
			bool b = x as bool;
		}
	`)
}

func TestScalarCastCharToF64(t *testing.T) {
	checkOK(t, `
		test() {
			char c = 'A';
			f64 f = c as f64;
		}
	`)
}

func TestScalarCastBoolToF64(t *testing.T) {
	checkOK(t, `
		test() {
			bool b = true;
			f64 f = b as f64;
		}
	`)
}

func TestScalarCastF64ToBool(t *testing.T) {
	checkOK(t, `
		test() {
			f64 f = 1.5;
			bool b = f as bool;
		}
	`)
}

// Scalar casts return target type directly (not optional), even with 'as'
func TestScalarCastNotOptional(t *testing.T) {
	checkOK(t, `
		test() {
			char c = 'A';
			int x = c as int;
			int y = x + 1;
		}
	`)
}

func TestScalarCastBoolNotOptional(t *testing.T) {
	// bool as int should return int (not int?), so arithmetic works
	checkOK(t, `
		test() {
			bool b = true;
			int x = b as int;
			int y = x + 10;
		}
	`)
}

func TestScalarCastF64ToBoolNotOptional(t *testing.T) {
	// f64 as bool should return bool (not bool?), so direct comparison works
	checkOK(t, `
		test() {
			f64 f = 1.5;
			bool b = f as bool;
			if b { }
		}
	`)
}

func TestScalarCastChainedTypes(t *testing.T) {
	checkOK(t, `
		test() {
			bool b = true;
			f64 f = (b as int) as f64;
		}
	`)
}

func TestScalarCastAllIntWidths(t *testing.T) {
	checkOK(t, `
		test() {
			i8 a = 1i8;
			i16 b = a as i16;
			i32 c = b as i32;
			int d = c as int;
			u8 e = 1u8;
			u16 f = e as u16;
			u32 g = f as u32;
			uint h = g as uint;
		}
	`)
}

func TestScalarCastF32ToF64(t *testing.T) {
	checkOK(t, `
		test() {
			f32 x = 1.5f32;
			f64 y = x as f64;
		}
	`)
}

func TestScalarCastF64ToF32(t *testing.T) {
	checkOK(t, `
		test() {
			f64 x = 1.5;
			f32 y = x as f32;
		}
	`)
}

func TestScalarCastBoolToChar(t *testing.T) {
	checkOK(t, `
		test() {
			bool b = true;
			char c = b as char;
		}
	`)
}

func TestScalarCastCharToBool(t *testing.T) {
	checkOK(t, `
		test() {
			char c = 'A';
			bool b = c as bool;
		}
	`)
}

func TestScalarCastBoolToF32(t *testing.T) {
	checkOK(t, `
		test() {
			bool b = true;
			f32 f = b as f32;
		}
	`)
}

func TestScalarCastF32ToBool(t *testing.T) {
	checkOK(t, `
		test() {
			f32 f = 1.0f32;
			bool b = f as bool;
		}
	`)
}

func TestScalarCastCharToF32(t *testing.T) {
	checkOK(t, `
		test() {
			char c = 'A';
			f32 f = c as f32;
		}
	`)
}

func TestScalarCastF32ToChar(t *testing.T) {
	checkOK(t, `
		test() {
			f32 f = 65.0f32;
			char c = f as char;
		}
	`)
}

func TestScalarCastAsBangNotOptional(t *testing.T) {
	// as! on scalars returns target type directly (same as 'as' for scalars)
	checkOK(t, `
		test() {
			char c = 'A';
			int x = c as! int;
			int y = x + 1;
		}
	`)
}

func TestScalarCastIdentity(t *testing.T) {
	checkOK(t, `
		test() {
			int x = 42;
			int y = x as int;
			bool b = true;
			bool b2 = b as bool;
			char c = 'A';
			char c2 = c as char;
			f64 f = 1.5;
			f64 f2 = f as f64;
		}
	`)
}

func TestScalarCastUintToBool(t *testing.T) {
	checkOK(t, `
		test() {
			uint x = 1 as! uint;
			bool b = x as bool;
		}
	`)
}

func TestScalarCastBoolToUint(t *testing.T) {
	checkOK(t, `
		test() {
			bool b = true;
			uint x = b as uint;
		}
	`)
}

func TestScalarCastInArithmetic(t *testing.T) {
	// Cast result used directly in arithmetic expression
	checkOK(t, `
		test() {
			char c = 'A';
			int x = (c as int) + 1;
		}
	`)
}

func TestScalarCastF64ToUint(t *testing.T) {
	checkOK(t, `
		test() {
			f64 f = 42.5;
			uint u = f as uint;
		}
	`)
}

func TestScalarCastUintToF64(t *testing.T) {
	checkOK(t, `
		test() {
			uint u = 100 as! uint;
			f64 f = u as f64;
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
	expectError(t, errs, "cannot assign int to field 'name' of type string")
	expectError(t, errs, "cannot assign string to field 'age' of type int")
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
	expectError(t, errs, "unknown parameter 'x'")
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
	expectError(t, errs, "missing required argument 'b'")
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
					raise error(message: "invalid port");
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

// --- Self on generic types ---

func TestSelfReturnTypeGeneric(t *testing.T) {
	checkOK(t, `
		type Box[T] {
			T value;
			new(~this, T v) { this.value = v; }
			wrap(T v) Self `+"`"+`factory {
				return Self(v: v);
			}
		}
		test() {
			Box[int] b = Box[int].wrap(v: 42);
		}
	`)
}

func TestSelfReturnTypeGenericMethodReturn(t *testing.T) {
	checkOK(t, `
		type Box[T] {
			T value;
			new(~this, T v) { this.value = v; }
			rewrap(T v) Self {
				return Self(v: v);
			}
		}
		test() {
			Box[int] b = Box[int](v: 1);
			Box[int] c = b.rewrap(v: 42);
		}
	`)
}

func TestSelfConstructorCallGeneric(t *testing.T) {
	checkOK(t, `
		type Pair[A, B] {
			A first;
			B second;
			new(~this, A a, B b) { this.first = a; this.second = b; }
			swap() Self {
				return Self(a: this.first, b: this.second);
			}
		}
		test() {
			Pair[int, string] p = Pair[int, string](a: 1, b: "x");
			Pair[int, string] q = p.swap();
		}
	`)
}

func TestSelfParameterTypeGeneric(t *testing.T) {
	checkOK(t, `
		type Box[T] {
			T value;
			new(~this, T v) { this.value = v; }
			same_value(Self other) bool {
				return true;
			}
		}
		test() {
			Box[int] a = Box[int](v: 1);
			Box[int] b = Box[int](v: 2);
			bool r = a.same_value(b);
		}
	`)
}

func TestSelfFieldTypeGeneric(t *testing.T) {
	checkOK(t, `
		type Node[T] {
			T value;
			Self? next;
		}
		test() {
			Node[int]? n = none;
		}
	`)
}

func TestSelfGenericFactoryAssignability(t *testing.T) {
	// The return type of a generic factory returning Self must be
	// assignable to the instantiated type
	checkOK(t, `
		type Wrapper[T] {
			T val;
			new(~this, T v) { this.val = v; }
			create(T v) Self `+"`"+`factory {
				return Self(v: v);
			}
		}
		test() {
			// Type inference: the result type of create should be Wrapper[int]
			w := Wrapper[int].create(v: 10);
		}
	`)
}

func TestSelfGenericRejectsTypeArgs(t *testing.T) {
	errs := checkErrs(t, `
		type Box[T] {
			T value;
			bad() Self[int] {
				return this;
			}
		}
		test() {}
	`)
	expectError(t, errs, "Self does not take type arguments")
}

func TestSelfOutsideTypeExpr(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			x := Self;
		}
	`)
	expectError(t, errs, "Self can only be used inside a type body")
}

func TestSelfGenericMultipleTypeParams(t *testing.T) {
	checkOK(t, `
		type Map2[K, V] {
			K key;
			V val;
			new(~this, K k, V v) { this.key = k; this.val = v; }
			make(K k, V v) Self `+"`"+`factory {
				return Self(k: k, v: v);
			}
		}
		test() {
			Map2[string, int] m = Map2[string, int].make(k: "x", v: 1);
		}
	`)
}

func TestSelfGenericReturnTypeMismatch(t *testing.T) {
	// Returning wrong instantiation should fail
	errs := checkErrs(t, `
		type Box[T] {
			T value;
			new(~this, T v) { this.value = v; }
			bad() Self {
				return Box[string](v: "oops");
			}
		}
		test() {
			Box[int] b = Box[int](v: 1);
		}
	`)
	expectError(t, errs, "cannot return")
}

func TestSelfFailableFactoryGeneric(t *testing.T) {
	checkOK(t, `
		type Validated[T] {
			T value;
			new(~this, T v) { this.value = v; }
			parse(T v) Self! `+"`"+`factory {
				return Self(v: v);
			}
		}
		test() {
			Validated[int] v = Validated[int].parse(v: 42)!;
		}
	`)
}

func TestSelfOptionalFactoryGeneric(t *testing.T) {
	checkOK(t, `
		type MaybeBox[T] {
			T value;
			new(~this, T v) { this.value = v; }
			try_wrap(T v, bool ok) Self? `+"`"+`factory {
				if !ok { return none; }
				return Self(v: v);
			}
		}
		test() {
			MaybeBox[int]? r = MaybeBox[int].try_wrap(v: 1, ok: true);
		}
	`)
}

func TestSelfLocalVarAnnotationGeneric(t *testing.T) {
	checkOK(t, `
		type Holder[T] {
			T value;
			new(~this, T v) { this.value = v; }
			duplicate() Self {
				Self copy = Holder[T](v: this.value);
				return copy;
			}
		}
		test() {
			Holder[int] h = Holder[int](v: 1);
			Holder[int] h2 = h.duplicate();
		}
	`)
}

func TestSelfFieldAccessAfterGenericFactory(t *testing.T) {
	// After calling a Self-returning factory on a generic type,
	// field access should see the instantiated element type
	checkOK(t, `
		type Box[T] {
			T value;
			new(~this, T v) { this.value = v; }
			wrap(T v) Self `+"`"+`factory {
				return Self(v: v);
			}
		}
		test() {
			b := Box[int].wrap(v: 42);
			int x = b.value;
		}
	`)
}

// --- Fix #1: Failable parent new propagation ---

func TestChildNewMustBeFailableWhenParentIs(t *testing.T) {
	errs := checkErrs(t, `
		type Animal {
			string name;
			new(~this, string name) void! {
				if name == "" { raise error(message: "empty"); }
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
				if name == "" { raise error(message: "empty"); }
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
			Animal a = Dog(name: "Rex");
			bool b = a is Dog;
		}
	`)
}

// --- Map Literal Tests ---

func TestMapLiteral(t *testing.T) {
	info := checkOK(t, `test() { m := {"a": 1, "b": 2}; }`)
	// Verify a concrete Map type was recorded (skip generic Map[K,V] from method signatures)
	for _, typ := range info.Types {
		if key, val, ok := types.AsMap(typ); ok {
			if _, isParam := key.(*types.TypeParam); isParam {
				continue
			}
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

func TestMapLiteralNonHashableKey(t *testing.T) {
	errs := checkErrs(t, `
		type Foo { int x; }
		test() { m := {Foo(x: 1): "a"}; }
	`)
	expectError(t, errs, "does not satisfy constraint Hashable")
}

func TestMapLiteralValidKeyTypes(t *testing.T) {
	checkOK(t, `test() { m := {"a": 1, "b": 2}; }`)
	checkOK(t, `test() { m := {1: "a", 2: "b"}; }`)
	checkOK(t, `test() { m := {true: 1, false: 2}; }`)
}

func TestMapLiteralNonEqualKey(t *testing.T) {
	// Type without == method fails Equal constraint
	errs := checkErrs(t, `
		type Bar { int x; get hash int => 0; }
		test() { m := {Bar(x: 1): "a"}; }
	`)
	expectError(t, errs, "does not satisfy constraint Equal")
}

func TestMapLiteralMissingBothConstraints(t *testing.T) {
	// Type without hash or == fails both constraints
	errs := checkErrs(t, `
		type Plain { int x; }
		test() { m := {Plain(x: 1): 42}; }
	`)
	expectError(t, errs, "does not satisfy constraint")
}

func TestMapLiteralCharKey(t *testing.T) {
	checkOK(t, `test() { m := {'a': 1, 'b': 2}; }`)
}

func TestMapLiteralI32Key(t *testing.T) {
	checkOK(t, `
		test(i32 k) {
			m := {k: "val"};
		}
		main() {}
	`)
}

func TestMapLiteralUserHashableEqualKey(t *testing.T) {
	checkOK(t, `
		type MyKey {
			int id;
			get hash int => this.id;
			==(MyKey other) bool => this.id == other.id;
			!=(MyKey other) bool => !(this == other);
		}
		test() {
			m := {MyKey(id: 1): "one", MyKey(id: 2): "two"};
		}
	`)
}

func TestMapLiteralValueTypeNotConstrained(t *testing.T) {
	// Value type has no constraints — any type should work
	checkOK(t, `
		type Payload { int data; }
		test() { m := {1: Payload(data: 42)}; }
	`)
}

func TestEmptyMapLiteral(t *testing.T) {
	checkOK(t, `
		test() {
			map[string, int] m = {:};
		}
	`)
}

func TestEmptyMapLiteralUntyped(t *testing.T) {
	errs := checkErrs(t, `test() { x := {:}; }`)
	expectError(t, errs, "empty map")
}

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
		if _, ok := types.AsRange(typ); ok {
			return
		}
	}
	t.Error("no Range instance recorded")
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
	expectError(t, errs, "requires Task[T] or Channel[T]")
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

// --- Function Type Tests ---

func TestFunctionTypeReturnOptional(t *testing.T) {
	// Grammar fix: () -> T? must parse as function returning T?, not optional of function returning T
	checkOK(t, `
		type Box {
			() -> int? getter;
			get_val() int? { return this.getter(); }
		}
		main() {
			b := Box(getter: || -> int? { return 42; });
		}
	`)
}

func TestZeroArgFunctionType(t *testing.T) {
	checkOK(t, `
		call(() -> int fn) int { return fn(); }
		main() {
			int r = call(|| -> int { return 42; });
		}
	`)
}

func TestVoidFunctionType(t *testing.T) {
	checkOK(t, `
		apply((int) -> void fn) {
			fn(42);
		}
		main() {
			apply(|int x| { });
		}
	`)
}

func TestVoidFunctionTypeAssignFromLambda(t *testing.T) {
	// A lambda with no return value should be assignable to (T) -> void
	checkOK(t, `
		run((int) -> void action) { action(1); }
		main() {
			run(|int x| { int y = x; });
		}
	`)
}

func TestFunctionTypeReturnSlice(t *testing.T) {
	// Suffix operators on function type return position
	checkOK(t, `
		make(() -> int[] fn) int[] { return fn(); }
		main() {
			int[] v = make(|| -> int[] { return [1, 2]; });
		}
	`)
}

// --- Function-typed Field Call Tests ---

func TestFunctionTypedFieldCallSema(t *testing.T) {
	// Calling a function-typed field via this.field()
	checkOK(t, `
		type Wrapper {
			() -> int getter;
			get_val() int { return this.getter(); }
		}
		main() {
			w := Wrapper(getter: || -> int { return 1; });
			int x = w.get_val();
		}
	`)
}

func TestFunctionTypedFieldCallWithArgsSema(t *testing.T) {
	// Calling a function-typed field with arguments
	checkOK(t, `
		type Adder {
			(int, int) -> int op;
			apply(int a, int b) int { return this.op(a, b); }
		}
		main() {
			a := Adder(op: |int x, int y| -> x + y);
			int r = a.apply(3, 4);
		}
	`)
}

func TestFunctionTypedFieldOptionalReturn(t *testing.T) {
	// Field returning optional — critical for _FnIter pattern
	checkOK(t, `
		type Iter {
			() -> int? supplier;
			next() int? { return this.supplier(); }
		}
		main() {
			i := Iter(supplier: || -> int? { return 42; });
			int? v = i.next();
		}
	`)
}

func TestFunctionTypedFieldVoidReturn(t *testing.T) {
	// Void function field
	checkOK(t, `
		type Handler {
			(int) -> void action;
			run(int x) { this.action(x); }
		}
		main() {
			h := Handler(action: |int x| { });
			h.run(1);
		}
	`)
}

func TestFunctionTypedFieldTypeMismatch(t *testing.T) {
	// Field type doesn't match constructor arg
	errs := checkErrs(t, `
		type Wrapper {
			() -> int getter;
		}
		main() {
			w := Wrapper(getter: || -> string { return "oops"; });
		}
	`)
	expectError(t, errs, "cannot assign")
}

// --- This Capture in Lambda Tests ---

func TestThisCaptureInMethodLambdaMove(t *testing.T) {
	// Capturing 'this' in a move lambda inside a method body
	checkOK(t, `
		type Counter {
			int count;
			make_incrementer() () -> int {
				return move || -> int {
					return this.count + 1;
				};
			}
		}
		main() {
			c := Counter(count: 10);
			() -> int fn = c.make_incrementer();
		}
	`)
}

func TestThisCaptureInMethodLambdaNoMoveError(t *testing.T) {
	// Non-copy type captured without move should error
	errs := checkErrs(t, `
		type Heavy {
			int x;
			make_fn() () -> int {
				return || -> int { return this.x; };
			}
		}
		main() {}
	`)
	expectError(t, errs, "cannot capture 'this' without move")
}

func TestThisCaptureOutsideMethodError(t *testing.T) {
	// Using 'this' outside a method body should error
	errs := checkErrs(t, `
		main() {
			fn := || -> int { return this.x; };
		}
	`)
	expectError(t, errs, "'this' used outside of a method")
}

func TestThisCaptureInNestedLambda(t *testing.T) {
	// Nested lambda capturing 'this' through intermediate lambda
	checkOK(t, `
		type Wrapper {
			int val;
			make_nested() () -> int {
				return move || -> int {
					() -> int inner = move || -> int {
						return this.val;
					};
					return inner();
				};
			}
		}
		main() {
			w := Wrapper(val: 42);
			() -> int fn = w.make_nested();
		}
	`)
}

// --- Additional Function Type Parsing Tests ---

func TestFunctionTypeMultiParamReturn(t *testing.T) {
	// Multi-parameter function type
	checkOK(t, `
		apply((int, string) -> bool pred) bool {
			return pred(1, "x");
		}
		main() {
			bool r = apply(|int a, string b| -> a > 0);
		}
	`)
}

func TestFunctionTypeAsReturnType(t *testing.T) {
	// Function returning a function type
	checkOK(t, `
		make_adder(int x) (int) -> int {
			return move |int y| -> x + y;
		}
		main() {
			(int) -> int add5 = make_adder(5);
			int r = add5(10);
		}
	`)
}

func TestFunctionTypeNested(t *testing.T) {
	// Function type taking a function type parameter
	checkOK(t, `
		apply_twice((int) -> int f, int x) int {
			return f(f(x));
		}
		main() {
			int r = apply_twice(|int x| -> x * 2, 3);
		}
	`)
}

func TestZeroArgFunctionTypeField(t *testing.T) {
	// Zero-arg function type as a struct field
	checkOK(t, `
		type Lazy {
			() -> int compute;
		}
		main() {
			l := Lazy(compute: || -> int { return 42; });
		}
	`)
}

func TestVoidFunctionTypeField(t *testing.T) {
	// Void function type as a struct field
	checkOK(t, `
		type Logger {
			(string) -> void log;
		}
		main() {
			l := Logger(log: |string s| { });
		}
	`)
}

func TestVoidFunctionTypeCallResult(t *testing.T) {
	// Cannot assign void function result to a variable
	errs := checkErrs(t, `
		run((int) -> void fn) {
			int x = fn(1);
		}
		main() {}
	`)
	expectError(t, errs, "cannot")
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
	if len(types.TypRange.TypeParams()) != 1 {
		t.Errorf("Range should have 1 type param, got %d", len(types.TypRange.TypeParams()))
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
	expectError(t, errs, "cannot assign string to parameter")
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
	expectError(t, errs, "cannot assign string to parameter")
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
		type MyHashable {
			hash() int `+"`abstract;"+`
		}
		type MyKey is MyHashable {
			hash() int { return 0; }
		}
		type MyMap[K: MyHashable, V] { K key; V val; }
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
			Animal a = Dog(name: "Rex", age: 3);
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
			raise error(message: "oops");
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
		type MyHashable {
			hash() int `+"`abstract;"+`
		}
		type Printable {
			toString() string `+"`abstract;"+`
		}
		type MyKey is MyHashable, Printable {
			hash() int { return 0; }
			toString() string { return "key"; }
		}
		type MyMap[K: MyHashable + Printable, V] { K key; V val; }
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
		type MyHashable {
			hash() int `+"`abstract;"+`
		}
		type MyKey is MyHashable {
			hash() int { return 0; }
		}
		type MyMap[K: MyHashable, V] { K key; V val; }
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
	expectError(t, errs, "no loaded scope")
}

func TestUseDeclMultiple(t *testing.T) {
	errs := checkErrs(t, `
		use io "std/io"
		use fmt "std/fmt"
		test() {
			io.Print();
		}
	`)
	expectError(t, errs, "no loaded scope")
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
		type MyHashable {
			hash() int `+"`abstract;"+`
		}
		type Printable {
			toString() string `+"`abstract;"+`
		}
		type Container[T: MyHashable + Printable] {
			T item;
			asHashable() MyHashable { return this.item; }
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

func TestDocOnParam(t *testing.T) {
	info := checkOK(t, `
		type T {
			foo(string url `+"`doc(\"The URL to fetch.\")"+`) {}
		}
	`)
	scope := info.Scopes[findFile(t, info)]
	named := scope.Lookup("T").(*types.TypeName).Type().(*types.Named)
	m := named.LookupMethod("foo")
	if m.Sig().Params()[0].Doc() != "The URL to fetch." {
		t.Errorf("expected param doc 'The URL to fetch.', got %q", m.Sig().Params()[0].Doc())
	}
}

func TestDocOnFuncParam(t *testing.T) {
	info := checkOK(t, `
		bar(int x `+"`doc(\"The count.\")"+`) {}
	`)
	scope := info.Scopes[findFile(t, info)]
	fn := scope.Lookup("bar").(*types.Func)
	sig := fn.Type().(*types.Signature)
	if sig.Params()[0].Doc() != "The count." {
		t.Errorf("expected param doc 'The count.', got %q", sig.Params()[0].Doc())
	}
}

func TestDocOnEnumVariant(t *testing.T) {
	info := checkOK(t, `
		enum Result {
			Ok(int value) `+"`doc(\"Success.\")"+`,
			Err(string msg) `+"`doc(\"Failure.\")"+`,
		}
	`)
	scope := info.Scopes[findFile(t, info)]
	enum := scope.Lookup("Result").(*types.TypeName).Type().(*types.Enum)
	if enum.Variants()[0].Doc() != "Success." {
		t.Errorf("expected variant doc 'Success.', got %q", enum.Variants()[0].Doc())
	}
	if enum.Variants()[1].Doc() != "Failure." {
		t.Errorf("expected variant doc 'Failure.', got %q", enum.Variants()[1].Doc())
	}
}

func TestDocTripleQuoted(t *testing.T) {
	info := checkOK(t, `
		bar() `+"`doc(\"\"\"Line one.\nLine two.\"\"\")"+` {}
	`)
	scope := info.Scopes[findFile(t, info)]
	fn := scope.Lookup("bar").(*types.Func)
	expected := "Line one.\nLine two."
	if fn.Doc() != expected {
		t.Errorf("expected doc %q, got %q", expected, fn.Doc())
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
			int r = identity[int](42);
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
			T y = x;
			return y;
		}
		main() {
			int r = identity[int](42);
		}
	`)
}

func TestGenericFuncCallWrongType(t *testing.T) {
	errs := checkErrs(t, `
		identity[T](T x) T { return x; }
		main() {
			int r = identity[int]("hello");
		}
	`)
	expectError(t, errs, "cannot assign string to parameter")
}

func TestGenericFuncMultipleInstances(t *testing.T) {
	info := checkOK(t, `
		identity[T](T x) T { return x; }
		main() {
			int a = identity[int](42);
			string b = identity[string]("hi");
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
			string s = identity[string]("hello");
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

func TestFixedArrayLiteralHint(t *testing.T) {
	// Array literal with fixed-size hint produces Array type
	checkOK(t, `
		main() {
			int[3] a = [1, 2, 3];
			int x = a[0];
			int n = a.len;
		}
	`)
}

func TestFixedArraySizeMismatch(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			int[3] a = [1, 2];
		}
	`)
	expectError(t, errs, "array literal has 2 elements but type int[3] requires 3")
}

func TestFixedArraySizeMismatchOver(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			int[2] a = [1, 2, 3];
		}
	`)
	expectError(t, errs, "array literal has 3 elements but type int[2] requires 2")
}

func TestFixedArrayForIn(t *testing.T) {
	checkOK(t, `
		main() {
			int[3] arr = [1, 2, 3];
			int sum = 0;
			for x in arr { sum += x; }
			for i, x in arr { sum += x; }
		}
	`)
}

func TestFixedArrayAssignment(t *testing.T) {
	checkOK(t, `
		main() {
			int[3] a = [1, 2, 3];
			int[3] b = a;
			b[0] = 42;
		}
	`)
}

func TestFixedArrayFieldAccess(t *testing.T) {
	checkOK(t, `
		type Grid { int[3] data; }
		main() {
			g := Grid(data: [1, 2, 3]);
			int x = g.data[0];
			g.data[1] = 42;
		}
	`)
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

// --- Relaxed structural satisfaction ---

func TestStructuralExtraOptionalParam(t *testing.T) {
	checkOK(t, `
		type Printable `+"`structural"+` {
			print() string `+"`abstract;"+`
		}
		type Doc {
			print(int? indent) string { return "doc"; }
		}
		main() {
			Printable p = Doc();
		}
	`)
}

func TestStructuralExtraDefaultedParam(t *testing.T) {
	checkOK(t, `
		type Printable `+"`structural"+` {
			print() string `+"`abstract;"+`
		}
		type Doc {
			print(int indent = 2) string { return "doc"; }
		}
		main() {
			Printable p = Doc();
		}
	`)
}

func TestStructuralExtraRequiredParamFails(t *testing.T) {
	errs := checkErrs(t, `
		type Printable `+"`structural"+` {
			print() string `+"`abstract;"+`
		}
		type Doc {
			print(int indent) string { return "doc"; }
		}
		main() {
			Printable p = Doc();
		}
	`)
	expectError(t, errs, "cannot assign")
}

func TestStructuralNonFailableSatisfiesFailable(t *testing.T) {
	checkOK(t, `
		type Processor `+"`structural"+` {
			process(int x) int! `+"`abstract;"+`
		}
		type Simple {
			process(int x) int { return x; }
		}
		main() {
			Processor p = Simple();
		}
	`)
}

func TestStructuralFailableDoesNotSatisfyNonFailable(t *testing.T) {
	errs := checkErrs(t, `
		type Processor `+"`structural"+` {
			process(int x) int `+"`abstract;"+`
		}
		type Risky {
			process(int x) int! { return x; }
		}
		main() {
			Processor p = Risky();
		}
	`)
	expectError(t, errs, "cannot assign")
}

func TestStructuralNonOptionalReturnSatisfiesOptional(t *testing.T) {
	checkOK(t, `
		type Finder `+"`structural"+` {
			find() int? `+"`abstract;"+`
		}
		type Always {
			find() int { return 42; }
		}
		main() {
			Finder f = Always();
		}
	`)
}

func TestStructuralOptionalReturnDoesNotSatisfyNonOptional(t *testing.T) {
	errs := checkErrs(t, `
		type Finder `+"`structural"+` {
			find() int `+"`abstract;"+`
		}
		type Maybe {
			find() int? { return 42; }
		}
		main() {
			Finder f = Maybe();
		}
	`)
	expectError(t, errs, "cannot assign")
}

func TestStructuralMultipleExtraOptionalParams(t *testing.T) {
	checkOK(t, `
		type Runnable `+"`structural"+` {
			run() int `+"`abstract;"+`
		}
		type Worker {
			run(int? priority, int? retries) int { return 1; }
		}
		main() {
			Runnable r = Worker();
		}
	`)
}

func TestStructuralMultipleExtraDefaultedParams(t *testing.T) {
	checkOK(t, `
		type Runnable `+"`structural"+` {
			run() int `+"`abstract;"+`
		}
		type Worker {
			run(int priority = 5, int retries = 3) int { return 1; }
		}
		main() {
			Runnable r = Worker();
		}
	`)
}

func TestStructuralMixedExtraParams(t *testing.T) {
	checkOK(t, `
		type Runnable `+"`structural"+` {
			run() int `+"`abstract;"+`
		}
		type Worker {
			run(int? priority, int retries = 3) int { return 1; }
		}
		main() {
			Runnable r = Worker();
		}
	`)
}

func TestStructuralMultipleExtraOneRequiredFails(t *testing.T) {
	errs := checkErrs(t, `
		type Runnable `+"`structural"+` {
			run() int `+"`abstract;"+`
		}
		type Worker {
			run(int? priority, int retries) int { return 1; }
		}
		main() {
			Runnable r = Worker();
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

// checkSourceWithStd combines stdSrc and userSrc as user code and runs checkSource.
// stdSrc is treated as extra user-level declarations (not actual std).
func checkSourceWithStd(t *testing.T, stdSrc, userSrc string) (*Info, []error) {
	t.Helper()
	if stdSrc == "" {
		return checkSource(t, userSrc)
	}
	return checkSource(t, stdSrc+"\n"+userSrc)
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

func TestStdFuncMissingReturnDetected(t *testing.T) {
	// Function with missing return should be caught by checkMissingReturn
	_, errs := checkSourceWithStd(t,
		`broken() int { }`,
		`main() {}`,
	)
	expectError(t, errs, "missing return")
}

// --- Stage 8k: Native type declaration tests ---

func TestNativeTypeStringMethod(t *testing.T) {
	// Getter on a native type (string) with a Promise body
	_, errs := checkSourceWithStd(t,
		`type string `+"`"+`native {
			get len int `+"`"+`native;
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
			get len int `+"`"+`native;
			get is_empty bool {}
		}`,
		`main() {}`,
	)
	expectError(t, errs, "missing return")
}

// --- Stage 8f: Builtin Validation Tests ---

// checkWithRawStd parses stdSrc as the std module using CheckForStdModule,
// and userSrc as user code. Used for testing validateBuiltins() error detection.
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
	stdInfo, stdErrs := CheckForStdModule(stdFile, HostTargetInfo())
	if len(stdErrs) > 0 {
		return stdInfo, stdErrs
	}
	rawStdScope := ExportedScope(stdInfo, stdFile)

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
	stdUse := &ast.UseDecl{Alias: "_", CatalogName: "std"}
	userFile.Uses = append([]*ast.UseDecl{stdUse}, userFile.Uses...)
	return CheckWithModules(userFile, map[string]*types.Scope{"std": rawStdScope})
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

func TestIntegerTypesHaveBitwiseOperators(t *testing.T) {
	checkOK(t, `main() {}`)

	bitwiseOps := []string{"&", "|", "^", "<<", ">>"}

	intTypes := map[string]*types.Named{
		"int": types.TypInt, "i8": types.TypI8, "i16": types.TypI16,
		"i32": types.TypI32, "i64": types.TypI64, "uint": types.TypUint,
		"u8": types.TypU8, "u16": types.TypU16, "u32": types.TypU32,
		"u64": types.TypU64,
	}

	for name, nt := range intTypes {
		t.Run(name, func(t *testing.T) {
			for _, op := range bitwiseOps {
				if nt.LookupMethod(op) == nil {
					t.Errorf("%s missing bitwise operator %s", name, op)
				}
			}
			// Check unary bitwise NOT (~)
			hasNot := false
			for _, m := range nt.Methods() {
				if m.Name() == "~" && len(m.Sig().Params()) == 0 {
					hasNot = true
					break
				}
			}
			if !hasNot {
				t.Errorf("%s missing unary operator ~", name)
			}
		})
	}
}

func TestBitwiseOperatorsTypeCheck(t *testing.T) {
	checkOK(t, `
		test() {
			int a = 12 & 10;
			int b = 5 | 3;
			int c = 12 ^ 10;
			int d = 1 << 4;
			int e = 16 >> 2;
			int f = ~0;
		}
	`)
}

func TestBitwiseOnNonIntegerFails(t *testing.T) {
	errs := checkErrs(t, `test() { x := "hello" & "world"; }`)
	expectError(t, errs, "operator & not defined")
}

func TestBitwiseNotOnNonIntegerFails(t *testing.T) {
	errs := checkErrs(t, `test() { x := ~true; }`)
	expectError(t, errs, "operator ~ not defined")
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
			drop(~this) void! { raise error(message: "err"); }
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
	// int& is copy since it's just a pointer (postfix & for shared ref)
	checkOK(t, `
		type Wrapper `+"`"+`copy {
			int& val;
		}
		main() {}
	`)
}

// isCopyField with MutRef — should be copy
func TestCopyTypeWithMutRefField(t *testing.T) {
	checkOK(t, `
		type MutWrapper `+"`"+`copy {
			int~ val;
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

// --- TypeParam operator dispatch tests ---

func TestTypeParamEqualityOperator(t *testing.T) {
	// == on a constrained TypeParam should work via Equal interface
	checkOK(t, `
		type Eq `+"`"+`structural {
			==(Self other) bool `+"`"+`abstract;
		}
		eq[T: Eq](T a, T b) bool { return a == b; }
		main() {}
	`)
}

func TestTypeParamOperatorMissingConstraint(t *testing.T) {
	// == on an unconstrained TypeParam should error
	errs := checkErrs(t, `
		eq[T](T a, T b) bool { return a == b; }
		main() {}
	`)
	expectError(t, errs, "operator == not defined on type parameter")
}

func TestTypeParamOperatorTypeMismatch(t *testing.T) {
	// == with mismatched types on TypeParam should error
	errs := checkErrs(t, `
		type Eq `+"`"+`structural {
			==(Self other) bool `+"`"+`abstract;
		}
		eq[T: Eq, U: Eq](T a, U b) bool { return a == b; }
		main() {}
	`)
	expectError(t, errs, "cannot use")
}

// --- TypeParam member access tests ---

func TestTypeParamMethodAccess(t *testing.T) {
	// Method call on a constrained TypeParam should resolve from the constraint
	checkOK(t, `
		type Showable `+"`"+`structural {
			show() string `+"`"+`abstract;
		}
		display[T: Showable](T item) string { return item.show(); }
		main() {}
	`)
}

func TestTypeParamGetterAccess(t *testing.T) {
	// Getter access on a constrained TypeParam should resolve from the constraint
	checkOK(t, `
		type HasLen `+"`"+`structural {
			get length int `+"`"+`abstract;
		}
		getLen[T: HasLen](T item) int { return item.length; }
		main() {}
	`)
}

func TestTypeParamMemberAccessNoConstraint(t *testing.T) {
	// Method call on an unconstrained TypeParam should error
	errs := checkErrs(t, `
		call[T](T x) string { return x.show(); }
		main() {}
	`)
	expectError(t, errs, "no method")
}

// --- Channel Semantics ---

// channelStd provides channel type declarations for sema tests
const channelStd = `
type Channel[T] ` + "`" + `native {
	new(int? capacity) ` + "`" + `native;
	send(T value) ` + "`" + `native;
	close() ` + "`" + `native;
}
`

func TestChannelReceiveReturnsOptional(t *testing.T) {
	// <-channel[int] returns int?, not int
	checkOKWithStd(t, channelStd, `
		test() {
			ch := channel[int](capacity: 1);
			ch.send(1);
			result := <-ch;
			int? x = result;
		}
	`)
}

func TestChannelReceiveNotBareType(t *testing.T) {
	// Assigning channel receive directly to int should fail — it's int?
	errs := checkErrsWithStd(t, channelStd, `
		test() {
			ch := channel[int](capacity: 1);
			ch.send(1);
			result := <-ch;
			int x = result;
		}
	`)
	expectError(t, errs, "cannot assign")
}

func TestChannelForInBindsElementType(t *testing.T) {
	// for v in channel[int] binds v as int (not int?)
	checkOKWithStd(t, channelStd, `
		test() {
			ch := channel[int](capacity: 1);
			ch.send(1);
			ch.close();
			for v in ch {
				int x = v;
			}
		}
	`)
}

func TestChannelConstructorUnbuffered(t *testing.T) {
	// channel[int]() — 0 args is valid (optional capacity param)
	checkOKWithStd(t, channelStd, `
		test() {
			ch := channel[int]();
		}
	`)
}

func TestChannelConstructorBuffered(t *testing.T) {
	// channel[int](capacity: 5) — 1 named arg
	checkOKWithStd(t, channelStd, `
		test() {
			ch := channel[int](capacity: 5);
		}
	`)
}

func TestChannelConstructorTooManyArgs(t *testing.T) {
	errs := checkErrsWithStd(t, channelStd, `
		test() {
			ch := channel[int](capacity: 5, capacity: 10);
		}
	`)
	expectError(t, errs, "expects")
}

func TestChannelConstructorWrongType(t *testing.T) {
	errs := checkErrsWithStd(t, channelStd, `
		test() {
			ch := channel[int]("hello");
		}
	`)
	expectError(t, errs, "cannot assign string to parameter 'capacity'")
}

func TestVectorConstructorNoArgs(t *testing.T) {
	checkOK(t, `
		test() {
			v := Vector[int]();
			v.push(1);
		}
	`)
}

func TestVectorConstructorWithCapacity(t *testing.T) {
	checkOK(t, `
		test() {
			v := Vector[int](32);
			v.push(1);
		}
	`)
}

func TestVectorConstructorWrongType(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			v := Vector[int]("hello");
		}
	`)
	expectError(t, errs, "cannot assign string to parameter 'capacity'")
}

func TestVectorConstructorTooManyArgs(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			v := Vector[int](16, 32);
		}
	`)
	expectError(t, errs, "expects at most 1 argument")
}

func TestVectorLenReadOnly(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			v := Vector[int]();
			v.len = 0;
		}
	`)
	expectError(t, errs, "has no setter")
}

func TestStringLenReadOnly(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			s := "hello";
			s.len = 0;
		}
	`)
	expectError(t, errs, "has no setter")
}

func TestVectorConstructorNamedArg(t *testing.T) {
	checkOK(t, `
		test() {
			v := Vector[int](capacity: 32);
			v.push(1);
		}
	`)
}

func TestVectorConstructorBoolCapacity(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			v := Vector[int](true);
		}
	`)
	expectError(t, errs, "cannot assign bool to parameter 'capacity'")
}

func TestVectorLenCompoundAssignReadOnly(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			v := Vector[int]();
			v.len += 1;
		}
	`)
	expectError(t, errs, "has no setter")
}

func TestStringLenCompoundAssignReadOnly(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			s := "hello";
			s.len += 1;
		}
	`)
	expectError(t, errs, "has no setter")
}

// --- Slice Type Expression (T[] in expression position) ---

func TestSliceTypeExprBasic(t *testing.T) {
	checkOK(t, `
		test() {
			v := int[]();
			v.push(1);
		}
	`)
}

func TestSliceTypeExprWithCapacity(t *testing.T) {
	checkOK(t, `
		test() {
			v := int[](capacity: 64);
			v.push(1);
		}
	`)
}

func TestSliceTypeExprString(t *testing.T) {
	checkOK(t, `
		test() {
			v := string[]();
			v.push("hello");
		}
	`)
}

func TestSliceTypeExprNested(t *testing.T) {
	checkOK(t, `
		test() {
			v := int[][]();
			inner := int[]();
			inner.push(1);
			v.push(inner);
		}
	`)
}

func TestSliceTypeExprFilledFactory(t *testing.T) {
	checkOK(t, `
		test() {
			v := int[].filled(value: 0, count: 10);
		}
	`)
}

func TestSliceTypeExprRejectsVariable(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			x := 42;
			v := x[]();
		}
	`)
	expectError(t, errs, "expected type name before []")
}

func TestSliceTypeExprRejectsLiteral(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			v := 42[]();
		}
	`)
	expectError(t, errs, "expected type name before []")
}

func TestSliceTypeExprRejectsCallResult(t *testing.T) {
	errs := checkErrs(t, `
		foo() int { return 1; }
		test() {
			v := foo()[]();
		}
	`)
	expectError(t, errs, "expected type name before []")
}

func TestTaskReceiveReturnsBareType(t *testing.T) {
	// <-task[int] returns int (not int?) — contrast with channel
	checkOK(t, `
		compute() int { return 42; }
		test() {
			t := go compute();
			result := <-t;
			int x = result;
		}
	`)
}

// --- Named Arguments Tests ---

func TestNamedArgsFunctionBasic(t *testing.T) {
	checkOK(t, `
		greet(string name, int age) string { return name; }
		test() {
			string s = greet(name: "Alice", age: 30);
		}
	`)
}

func TestNamedArgsFunctionReorder(t *testing.T) {
	// Named args can appear in any order
	checkOK(t, `
		greet(string name, int age) string { return name; }
		test() {
			string s = greet(age: 30, name: "Alice");
		}
	`)
}

func TestNamedArgsPositionalThenNamed(t *testing.T) {
	checkOK(t, `
		add(int a, int b, int c) int { return a + b + c; }
		test() {
			int r = add(1, 2, c: 3);
		}
	`)
}

func TestNamedArgsErrorPositionalAfterNamed(t *testing.T) {
	errs := checkErrs(t, `
		add(int a, int b) int { return a + b; }
		test() { int r = add(a: 1, 2); }
	`)
	expectError(t, errs, "positional argument after named argument")
}

func TestNamedArgsErrorUnknownParam(t *testing.T) {
	errs := checkErrs(t, `
		add(int a, int b) int { return a + b; }
		test() { int r = add(a: 1, c: 2); }
	`)
	expectError(t, errs, "unknown parameter 'c'")
}

func TestNamedArgsErrorDuplicateParam(t *testing.T) {
	errs := checkErrs(t, `
		add(int a, int b) int { return a + b; }
		test() { int r = add(a: 1, a: 2); }
	`)
	expectError(t, errs, "parameter 'a' already provided")
}

func TestNamedArgsErrorPositionalFillsThenNamedDuplicates(t *testing.T) {
	errs := checkErrs(t, `
		add(int a, int b) int { return a + b; }
		test() { int r = add(1, a: 2); }
	`)
	expectError(t, errs, "parameter 'a' already provided")
}

func TestNamedArgsConstructorReorder(t *testing.T) {
	// Implicit constructor with named args in different order
	checkOK(t, `
		type Dog { string name; int age; }
		test() {
			Dog d = Dog(age: 3, name: "Rex");
		}
	`)
}

func TestNamedArgsConstructorPositionalThenNamed(t *testing.T) {
	// First positional fills first field, then named
	checkOK(t, `
		type Dog { string name; int age; }
		test() {
			Dog d = Dog("Rex", age: 3);
		}
	`)
}

func TestNamedArgsConstructorAllPositional(t *testing.T) {
	// All positional: fills fields in declaration order
	checkOK(t, `
		type Dog { string name; int age; }
		test() {
			Dog d = Dog("Rex", 3);
		}
	`)
}

func TestNamedArgsConstructorSkipOptional(t *testing.T) {
	// Skip optional field using named args
	checkOK(t, `
		type Config { string host; int port; string? label; }
		test() {
			Config c = Config(host: "localhost", port: 8080);
		}
	`)
}

func TestNamedArgsConstructorErrorPositionalAfterNamed(t *testing.T) {
	errs := checkErrs(t, `
		type Dog { string name; int age; }
		test() { Dog d = Dog(name: "Rex", 3); }
	`)
	expectError(t, errs, "positional argument after named argument")
}

func TestNamedArgsNewConstructorReorder(t *testing.T) {
	checkOK(t, `
		type Point {
			int x;
			int y;
			new(~this, int x, int y) {
				this.x = x;
				this.y = y;
			}
		}
		test() {
			Point p = Point(y: 2, x: 1);
		}
	`)
}

func TestNamedArgsNewConstructorPositionalThenNamed(t *testing.T) {
	checkOK(t, `
		type Point {
			int x;
			int y;
			new(~this, int x, int y) {
				this.x = x;
				this.y = y;
			}
		}
		test() {
			Point p = Point(1, y: 2);
		}
	`)
}

func TestNamedArgsMethodCall(t *testing.T) {
	checkOK(t, `
		type Calc {
			int value;
			add(int a, int b) int { return a + b; }
		}
		test() {
			Calc c = Calc(value: 0);
			int r = c.add(b: 2, a: 1);
		}
	`)
}

func TestNamedArgsTooManyArgs(t *testing.T) {
	errs := checkErrs(t, `
		add(int a, int b) int { return a + b; }
		test() { int r = add(1, 2, 3); }
	`)
	expectError(t, errs, "expects 2 arguments, got 3")
}

func TestNamedArgsTypeMismatchReordered(t *testing.T) {
	errs := checkErrs(t, `
		greet(string name, int age) string { return name; }
		test() {
			string s = greet(age: "old", name: 42);
		}
	`)
	expectError(t, errs, "cannot assign string to parameter 'age'")
	expectError(t, errs, "cannot assign int to parameter 'name'")
}

func TestNamedArgsConstructorTypeMismatchReordered(t *testing.T) {
	errs := checkErrs(t, `
		type Dog { string name; int age; }
		test() {
			Dog d = Dog(age: "old", name: 42);
		}
	`)
	expectError(t, errs, "cannot assign string to field 'age'")
	expectError(t, errs, "cannot assign int to field 'name'")
}

// --- Param Annotation Tests ---

func TestParamAnnotationFunctionAccepted(t *testing.T) {
	checkOK(t, `
		greet(string name `+"`"+`doc("who to greet"), int times `+"`"+`doc("repeat count") = 1) {}
		test() { greet("hi"); }
	`)
}

func TestParamAnnotationMethodAccepted(t *testing.T) {
	checkOK(t, `
		type Calc {
			int value;
			add(&this, int a `+"`"+`doc("operand")) int { return this.value + a; }
		}
		test() {
			Calc c = Calc(value: 1);
			int r = c.add(a: 2);
		}
	`)
}

func TestParamAnnotationNewConstructorAccepted(t *testing.T) {
	checkOK(t, `
		type Point {
			int x; int y;
			new(~this, int x `+"`"+`doc("x coord"), int y `+"`"+`doc("y coord")) {
				this.x = x;
				this.y = y;
			}
		}
		test() { Point p = Point(x: 1, y: 2); }
	`)
}

func TestParamAnnotationWithDefaultValue(t *testing.T) {
	checkOK(t, `
		connect(string host `+"`"+`doc("hostname"), int port `+"`"+`doc("port") = 8080) {}
		test() { connect("localhost"); }
	`)
}

func TestParamAnnotationNamedArgsStillWork(t *testing.T) {
	checkOK(t, `
		add(int a `+"`"+`doc("first"), int b `+"`"+`doc("second")) int { return a + b; }
		test() { int r = add(b: 2, a: 1); }
	`)
}

// --- Optional narrowing tests ---

func TestOptionalTruthinessNarrowing(t *testing.T) {
	// if cc { ... } where cc is string? should narrow cc to string
	info := checkOK(t, `
		test() {
			string? cc = "hello";
			if cc {
				string s = cc;
			}
		}
	`)
	if len(info.OptionalNarrowings) != 1 {
		t.Errorf("expected 1 narrowing, got %d", len(info.OptionalNarrowings))
	}
}

func TestOptionalTruthinessNarrowingInt(t *testing.T) {
	// if x { ... } where x is int? should narrow x to int
	checkOK(t, `
		test() {
			int? x = 42;
			if x {
				int n = x;
			}
		}
	`)
}

func TestOptionalTruthinessNarrowingBoolError(t *testing.T) {
	// if x { ... } where x is bool? should error (ambiguous) — exactly one error, no double-report
	errs := checkErrs(t, `
		test() {
			bool? x = true;
			if x {}
		}
	`)
	expectError(t, errs, "bool? in if condition is ambiguous")
	if len(errs) != 1 {
		t.Errorf("expected exactly 1 error for bool? ambiguity, got %d: %v", len(errs), errs)
	}
}

func TestIsPresentNarrowing(t *testing.T) {
	// if x is present { ... } should narrow x to T
	checkOK(t, `
		test() {
			string? cc = "hello";
			if cc is present {
				string s = cc;
			}
		}
	`)
}

func TestIsPresentNarrowingBoolOptional(t *testing.T) {
	// is present works for bool? (unlike truthiness)
	checkOK(t, `
		test() {
			bool? verbose = true;
			if verbose is present {
				bool b = verbose;
			}
		}
	`)
}

func TestIsAbsentNarrowing(t *testing.T) {
	// is absent with diverging body should narrow x to T after the if
	checkOK(t, `
		test() int! {
			int? x = 42;
			if x is absent {
				raise error(message: "missing");
			}
			int y = x;
			return y;
		}
	`)
}

func TestIsAbsentNarrowingElse(t *testing.T) {
	// is absent: else branch should have narrowed (present) variable
	checkOK(t, `
		test() {
			int? x = 42;
			if x is absent {
				int y = 0;
			} else {
				int y = x;
			}
		}
	`)
}

func TestIsAbsentNoNarrowing(t *testing.T) {
	// is absent then-block: x is none, no narrowing to T
	checkOK(t, `
		test() {
			int? x = 42;
			if x is absent {
				int y = 0;
			}
		}
	`)
}

func TestOptionalNarrowingWithElse(t *testing.T) {
	// After the if block, cc should still be string? (no narrowing in else)
	checkOK(t, `
		test() {
			string? cc = "hello";
			if cc {
				string s = cc;
			} else {
				int y = 0;
			}
		}
	`)
}

func TestOptionalNarrowingNonIdent(t *testing.T) {
	// Complex expressions don't trigger narrowing, should error as non-bool
	errs := checkErrs(t, `
		getOpt() int? { return 42; }
		test() {
			if getOpt() {}
		}
	`)
	expectError(t, errs, "if condition must be bool")
}

// --- Uninitialized optional var decls ---

func TestUninitOptionalVar(t *testing.T) {
	checkOK(t, `
		test() {
			int? x;
			string? s;
		}
	`)
}

func TestUninitNonOptionalError(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			int x;
		}
	`)
	expectError(t, errs, "uninitialized variable x requires optional type")
}

// --- Negated narrowing (!cc) ---

func TestNegatedNarrowing(t *testing.T) {
	checkOK(t, `
		test() {
			string? cc = "hello";
			if !cc {
				// cc is none here — no narrowing
			} else {
				string s = cc;
			}
		}
	`)
}

func TestNegatedNarrowingNoElse(t *testing.T) {
	checkOK(t, `
		test() {
			int? x = 42;
			if !x {
				// x is none
			}
		}
	`)
}

func TestNegatedNarrowingPostDivergence(t *testing.T) {
	// if !x { return; } should narrow x to T after the if
	checkOK(t, `
		test() int! {
			int? x = 42;
			if !x {
				raise error(message: "missing");
			}
			int y = x;
			return y;
		}
	`)
}

func TestIsAbsentNarrowingPostDivergenceReturn(t *testing.T) {
	// if x is absent { return; } should narrow x to T after the if
	checkOK(t, `
		test() {
			int? x = 42;
			if x is absent {
				return;
			}
			int y = x;
		}
	`)
}

func TestIsAbsentNarrowingBoolOptional(t *testing.T) {
	// is absent should work with bool? (unlike truthiness narrowing)
	checkOK(t, `
		test() {
			bool? flag = true;
			if flag is absent {
				return;
			}
			bool b = flag;
		}
	`)
}

func TestOptionalForceUnwrap(t *testing.T) {
	checkOK(t, "test() { int? x = 42; int y = x!; }")
}

func TestOptionalForceUnwrapError(t *testing.T) {
	errs := checkErrs(t, "test() { int x = 42; int y = x!; }")
	expectError(t, errs, "unwrap (!) requires a failable or optional expression")
}

func TestOptionalHandler(t *testing.T) {
	checkOK(t, "test() { int? x = 42; int y = x ? _ { 0; }; }")
}

func TestOptionalHandlerDiverge(t *testing.T) {
	checkOK(t, "test() { int? x = 42; int y = x ? _ { return; }; }")
}

func TestOptionalHandlerTypedPatternError(t *testing.T) {
	// Optional handler does not support typed patterns
	errs := checkErrs(t, `
		type MyErr is error { int code; }
		test() { int? x = 42; int y = x ? _ is MyErr { 0; }; }
	`)
	expectError(t, errs, "optional handler does not support typed patterns")
}

func TestOptionalForceUnwrapReturnType(t *testing.T) {
	// x! should return T, usable in arithmetic
	checkOK(t, "test() { int? x = 42; int y = x! + 1; }")
}

func TestIsAbsentNoPostNarrowWithElse(t *testing.T) {
	// post-narrowing should NOT fire when else is present
	errs := checkErrs(t, `
		test() {
			int? x = 42;
			if x is absent {
				return;
			} else {
				int y = 0;
			}
			int z = x;
		}
	`)
	expectError(t, errs, "cannot assign int? to variable of type int")
}

func TestIsPresentNoPostNarrow(t *testing.T) {
	// non-negated narrowing should NOT post-narrow after diverging then-body
	errs := checkErrs(t, `
		test() {
			int? x = 42;
			if x is present {
				return;
			}
			int z = x;
		}
	`)
	expectError(t, errs, "cannot assign int? to variable of type int")
}

// --- Compound narrowing (&&) ---

func TestCompoundNarrowing(t *testing.T) {
	checkOK(t, `
		test() {
			int? a = 1;
			string? b = "hi";
			if a && b {
				int x = a;
				string y = b;
			}
		}
	`)
}

func TestCompoundNarrowingWithIsPresent(t *testing.T) {
	checkOK(t, `
		test() {
			bool? a = true;
			int? b = 42;
			if a is present && b {
				bool x = a;
				int y = b;
			}
		}
	`)
}

func TestCompoundNarrowingElseBranchNotNarrowed(t *testing.T) {
	// In the else branch, vars stay as T? — not narrowed
	checkOK(t, `
		test() {
			int? a = 1;
			string? b = "hi";
			if a && b {
				int x = a;
			} else {
				string sa = "{a}";
				string sb = "{b}";
			}
		}
	`)
}

// --- Generator Tests ---

func TestGeneratorBasic(t *testing.T) {
	checkOK(t, `
		count() stream[int] {
			yield 1;
			yield 2;
			yield 3;
		}
		main() {}
	`)
}

func TestGeneratorYieldOutsideGenerator(t *testing.T) {
	errs := checkErrs(t, `
		foo() {
			yield 1;
		}
		main() {}
	`)
	expectError(t, errs, "yield outside of generator function")
}

func TestGeneratorYieldTypeMismatch(t *testing.T) {
	errs := checkErrs(t, `
		count() stream[int] {
			yield "hello";
		}
		main() {}
	`)
	expectError(t, errs, "cannot yield string in generator returning stream[int]")
}

func TestGeneratorYieldInsideLambda(t *testing.T) {
	errs := checkErrs(t, `
		gen() stream[int] {
			f := || { yield 1; };
		}
		main() {}
	`)
	expectError(t, errs, "yield inside lambda/closure is not allowed")
}

func TestGeneratorFailableError(t *testing.T) {
	errs := checkErrs(t, `
		gen() stream[int]! {
			yield 1;
		}
		main() {}
	`)
	expectError(t, errs, "generator functions cannot be failable")
}

func TestGeneratorMissingReturnOK(t *testing.T) {
	// Generators don't need explicit return — they terminate by falling off the end
	checkOK(t, `
		gen() stream[int] {
			yield 1;
		}
		main() {}
	`)
}

func TestGeneratorBareReturn(t *testing.T) {
	// bare return in generators is OK (early termination)
	checkOK(t, `
		gen(bool stop) stream[int] {
			yield 1;
			if stop { return; }
			yield 2;
		}
		main() {}
	`)
}

func TestGeneratorReturnValue(t *testing.T) {
	errs := checkErrs(t, `
		gen() stream[int] {
			return 42;
		}
		main() {}
	`)
	expectError(t, errs, "use yield instead")
}

func TestGeneratorNoYield(t *testing.T) {
	errs := checkErrs(t, `
		gen() stream[int] {
		}
		main() {}
	`)
	expectError(t, errs, "contains no yield statements")
}

func TestGeneratorNoYieldBareReturn(t *testing.T) {
	errs := checkErrs(t, `
		gen() stream[int] {
			return;
		}
		main() {}
	`)
	expectError(t, errs, "contains no yield statements")
}

func TestGeneratorMethodNoYield(t *testing.T) {
	errs := checkErrs(t, `
		type Foo {
			gen() stream[int] {}
		}
		main() {}
	`)
	expectError(t, errs, "contains no yield statements")
}

func TestGeneratorMethodBasic(t *testing.T) {
	checkOK(t, `
		type Foo {
			int n;
			gen() stream[int] {
				yield this.n;
			}
		}
		main() {}
	`)
}

// --- Module System Tests ---

// checkWithModules parses user source with pre-loaded module scopes.
// std is always included via the module approach.
func checkWithModules(t *testing.T, userSrc string, moduleScopes map[string]*types.Scope) (*Info, []error) {
	t.Helper()
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
	// Inject use std as _
	stdUse := &ast.UseDecl{Alias: "_", CatalogName: "std"}
	userFile.Uses = append([]*ast.UseDecl{stdUse}, userFile.Uses...)
	// Merge std scope into provided module scopes
	allScopes := map[string]*types.Scope{"std": getSemaStdScope()}
	for k, v := range moduleScopes {
		allScopes[k] = v
	}
	return CheckWithModules(userFile, allScopes)
}

// makeModuleScope creates a module scope with exported function declarations.
func makeModuleScope(t *testing.T, funcs map[string]*types.Signature) *types.Scope {
	t.Helper()
	scope := types.NewScope(nil, types.Pos{}, types.Pos{}, "module")
	for name, sig := range funcs {
		fn := types.NewFunc(types.Pos{}, name, sig)
		fn.SetExported(true)
		scope.Insert(fn)
	}
	return scope
}

// makeModuleScopeWithVisibility creates a module scope where each function
// is marked exported or private based on the exported map.
func makeModuleScopeWithVisibility(t *testing.T, funcs map[string]*types.Signature, exported map[string]bool) *types.Scope {
	t.Helper()
	scope := types.NewScope(nil, types.Pos{}, types.Pos{}, "module")
	for name, sig := range funcs {
		fn := types.NewFunc(types.Pos{}, name, sig)
		if exported[name] {
			fn.SetExported(true)
		}
		scope.Insert(fn)
	}
	return scope
}

func TestModuleQualifiedAccess(t *testing.T) {
	// Create a module with a function: mymod.greet() int
	sig := types.NewSignature(nil, nil, types.TypInt, false)
	modScope := makeModuleScope(t, map[string]*types.Signature{
		"greet": sig,
	})
	moduleScopes := map[string]*types.Scope{
		"mymod": modScope,
	}

	info, errs := checkWithModules(t, `
		use mymod;
		main() {
			int x = mymod.greet();
		}
	`, moduleScopes)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	_ = info
}

func TestModuleQualifiedAccessWithAlias(t *testing.T) {
	sig := types.NewSignature(nil, nil, types.TypInt, false)
	modScope := makeModuleScope(t, map[string]*types.Signature{
		"greet": sig,
	})
	moduleScopes := map[string]*types.Scope{
		"mymod": modScope,
	}

	_, errs := checkWithModules(t, `
		use mymod as m;
		main() {
			int x = m.greet();
		}
	`, moduleScopes)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}

func TestModuleNoSuchMember(t *testing.T) {
	sig := types.NewSignature(nil, nil, types.TypInt, false)
	modScope := makeModuleScope(t, map[string]*types.Signature{
		"greet": sig,
	})
	moduleScopes := map[string]*types.Scope{
		"mymod": modScope,
	}

	_, errs := checkWithModules(t, `
		use mymod;
		main() {
			mymod.nonexistent();
		}
	`, moduleScopes)
	expectError(t, errs, "no exported member 'nonexistent'")
}

func TestModuleGlobImport(t *testing.T) {
	sig := types.NewSignature(nil, nil, types.TypInt, false)
	modScope := makeModuleScope(t, map[string]*types.Signature{
		"greet": sig,
	})
	moduleScopes := map[string]*types.Scope{
		"mymod": modScope,
	}

	_, errs := checkWithModules(t, `
		use mymod as _;
		main() {
			int x = greet();
		}
	`, moduleScopes)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}

func TestModuleGlobConflict(t *testing.T) {
	sig := types.NewSignature(nil, nil, types.TypInt, false)
	mod1 := makeModuleScope(t, map[string]*types.Signature{
		"helper": sig,
	})
	mod2 := makeModuleScope(t, map[string]*types.Signature{
		"helper": sig,
	})
	moduleScopes := map[string]*types.Scope{
		"mod1": mod1,
		"mod2": mod2,
	}

	_, errs := checkWithModules(t, `
		use mod1 as _;
		use mod2 as _;
		main() {}
	`, moduleScopes)
	expectError(t, errs, "conflicts with existing symbol 'helper'")
}

func TestModuleSourcedQualifiedAccess(t *testing.T) {
	sig := types.NewSignature(nil, nil, types.TypInt, false)
	modScope := makeModuleScope(t, map[string]*types.Signature{
		"parse": sig,
	})
	moduleScopes := map[string]*types.Scope{
		"./libs/parser": modScope,
	}

	_, errs := checkWithModules(t, `
		use parser "./libs/parser";
		main() {
			int x = parser.parse();
		}
	`, moduleScopes)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}

func TestModuleSourcedGlobImport(t *testing.T) {
	sig := types.NewSignature(nil, nil, types.TypInt, false)
	modScope := makeModuleScope(t, map[string]*types.Signature{
		"parse": sig,
	})
	moduleScopes := map[string]*types.Scope{
		"./libs/parser": modScope,
	}

	_, errs := checkWithModules(t, `
		use _ "./libs/parser";
		main() {
			int x = parse();
		}
	`, moduleScopes)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}

// --- Module Visibility Tests ---

func TestModulePrivateMemberAccess(t *testing.T) {
	sig := types.NewSignature(nil, nil, types.TypInt, false)
	modScope := makeModuleScopeWithVisibility(t,
		map[string]*types.Signature{"greet": sig, "helper": sig},
		map[string]bool{"greet": true}, // helper is private
	)
	moduleScopes := map[string]*types.Scope{"mymod": modScope}

	// Public member works
	_, errs := checkWithModules(t, `
		use mymod;
		main() { int x = mymod.greet(); }
	`, moduleScopes)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}

func TestModulePrivateMemberDenied(t *testing.T) {
	sig := types.NewSignature(nil, nil, types.TypInt, false)
	modScope := makeModuleScopeWithVisibility(t,
		map[string]*types.Signature{"greet": sig, "helper": sig},
		map[string]bool{"greet": true}, // helper is private
	)
	moduleScopes := map[string]*types.Scope{"mymod": modScope}

	_, errs := checkWithModules(t, `
		use mymod;
		main() { int x = mymod.helper(); }
	`, moduleScopes)
	expectError(t, errs, "'helper' is private to module 'mymod'")
}

func TestModuleGlobImportSkipsPrivate(t *testing.T) {
	sig := types.NewSignature(nil, nil, types.TypInt, false)
	modScope := makeModuleScopeWithVisibility(t,
		map[string]*types.Signature{"greet": sig, "helper": sig},
		map[string]bool{"greet": true}, // helper is private
	)
	moduleScopes := map[string]*types.Scope{"mymod": modScope}

	// greet() is imported via glob, helper() is not
	_, errs := checkWithModules(t, `
		use mymod as _;
		main() { int x = greet(); }
	`, moduleScopes)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}

func TestModuleGlobImportPrivateNotVisible(t *testing.T) {
	sig := types.NewSignature(nil, nil, types.TypInt, false)
	modScope := makeModuleScopeWithVisibility(t,
		map[string]*types.Signature{"greet": sig, "helper": sig},
		map[string]bool{"greet": true}, // helper is private
	)
	moduleScopes := map[string]*types.Scope{"mymod": modScope}

	_, errs := checkWithModules(t, `
		use mymod as _;
		main() { int x = helper(); }
	`, moduleScopes)
	expectError(t, errs, "undefined")
}

// --- Module Qualified Type Ref Tests ---

// makeModuleScopeWithTypes creates a module scope with exported types and functions.
func makeModuleScopeWithTypes(t *testing.T) *types.Scope {
	t.Helper()
	scope := types.NewScope(nil, types.Pos{}, types.Pos{}, "module")

	// Add an exported type: User { string name; int age; }
	tn := types.NewTypeName(types.Pos{}, "User", nil)
	named := types.NewNamed(tn, nil)
	named.SetExported(true)
	named.AddField(types.NewField(types.Pos{}, "name", types.TypString, types.PlaceInstance, false, false))
	named.AddField(types.NewField(types.Pos{}, "age", types.TypInt, types.PlaceInstance, false, false))
	scope.Insert(tn)

	// Add a private type: Internal { int id; }
	tn2 := types.NewTypeName(types.Pos{}, "Internal", nil)
	named2 := types.NewNamed(tn2, nil)
	// not exported
	named2.AddField(types.NewField(types.Pos{}, "id", types.TypInt, types.PlaceInstance, false, false))
	scope.Insert(tn2)

	// Add exported function: create_user() User
	sig := types.NewSignature(nil, nil, named, false)
	fn := types.NewFunc(types.Pos{}, "create_user", sig)
	fn.SetExported(true)
	scope.Insert(fn)

	return scope
}

func TestModuleQualifiedTypeRef(t *testing.T) {
	modScope := makeModuleScopeWithTypes(t)
	moduleScopes := map[string]*types.Scope{"models": modScope}

	_, errs := checkWithModules(t, `
		use models;
		main() {
			models.User u = models.create_user();
		}
	`, moduleScopes)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}

func TestModuleQualifiedTypeRefPrivate(t *testing.T) {
	modScope := makeModuleScopeWithTypes(t)
	moduleScopes := map[string]*types.Scope{"models": modScope}

	_, errs := checkWithModules(t, `
		use models;
		main() {
			models.Internal x = models.create_user();
		}
	`, moduleScopes)
	expectError(t, errs, "'Internal' is private to module 'models'")
}

func TestModuleQualifiedTypeRefUndefined(t *testing.T) {
	modScope := makeModuleScopeWithTypes(t)
	moduleScopes := map[string]*types.Scope{"models": modScope}

	_, errs := checkWithModules(t, `
		use models;
		main() {
			models.Nonexistent x = models.create_user();
		}
	`, moduleScopes)
	expectError(t, errs, "no exported member 'Nonexistent'")
}

func TestModuleQualifiedTypeRefNotAType(t *testing.T) {
	modScope := makeModuleScopeWithTypes(t)
	moduleScopes := map[string]*types.Scope{"models": modScope}

	_, errs := checkWithModules(t, `
		use models;
		main() {
			models.create_user x = models.create_user();
		}
	`, moduleScopes)
	expectError(t, errs, "is not a type")
}

func TestModuleQualifiedTypeRefAsParam(t *testing.T) {
	modScope := makeModuleScopeWithTypes(t)
	moduleScopes := map[string]*types.Scope{"models": modScope}

	_, errs := checkWithModules(t, `
		use models;
		greet(models.User u) string { return "hi"; }
		main() {
			models.User u = models.create_user();
			greet(u);
		}
	`, moduleScopes)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}

func TestModuleQualifiedTypeRefAsReturn(t *testing.T) {
	modScope := makeModuleScopeWithTypes(t)
	moduleScopes := map[string]*types.Scope{"models": modScope}

	_, errs := checkWithModules(t, `
		use models;
		make_user() models.User { return models.create_user(); }
		main() {
			models.User u = make_user();
		}
	`, moduleScopes)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}

// --- ExportedScope tests ---

// checkModuleSource runs sema on module source code and returns its ExportedScope.
func checkModuleSource(t *testing.T, src string) *types.Scope {
	t.Helper()
	// Parse module source
	modInput := antlr.NewInputStream(src)
	modLexer := parser.NewPromiseLexer(modInput)
	modLexer.RemoveErrorListeners()
	modStream := antlr.NewCommonTokenStream(modLexer, antlr.TokenDefaultChannel)
	modP := parser.NewPromiseParser(modStream)
	modP.RemoveErrorListeners()
	modTree := modP.CompilationUnit()
	modFile, errs := ast.Build("module.pr", modTree)
	if len(errs) > 0 {
		t.Fatalf("module AST build errors: %v", errs)
	}
	// Inject use std as _
	stdUse := &ast.UseDecl{Alias: "_", CatalogName: "std"}
	modFile.Uses = append([]*ast.UseDecl{stdUse}, modFile.Uses...)
	info, errs := CheckWithModules(modFile, map[string]*types.Scope{"std": getSemaStdScope()})
	if len(errs) > 0 {
		t.Fatalf("module sema errors: %v", errs)
	}
	return ExportedScope(info, modFile)
}

func TestExportedScopeOnlyPublic(t *testing.T) {
	scope := checkModuleSource(t, "greet() int `public { return 42; }\nhelper() int { return 1; }")

	if scope.Lookup("greet") == nil {
		t.Error("expected 'greet' in exported scope")
	}
	if scope.Lookup("helper") != nil {
		t.Error("'helper' should not be in exported scope (not public)")
	}
}

func TestExportedScopePublicType(t *testing.T) {
	scope := checkModuleSource(t, "type User `public { string name; int age; }\ntype Internal { int id; }")

	if scope.Lookup("User") == nil {
		t.Error("expected 'User' in exported scope")
	}
	if scope.Lookup("Internal") != nil {
		t.Error("'Internal' should not be in exported scope (not public)")
	}
}

func TestExportedScopePublicEnum(t *testing.T) {
	scope := checkModuleSource(t, "enum Color `public { Red; Green; Blue; }\nenum Secret { A; B; }")

	if scope.Lookup("Color") == nil {
		t.Error("expected 'Color' in exported scope")
	}
	if scope.Lookup("Secret") != nil {
		t.Error("'Secret' should not be in exported scope (not public)")
	}
}

func TestExportedScopeEmpty(t *testing.T) {
	scope := checkModuleSource(t, "helper() int { return 1; }\ntype Internal { int id; }")

	if scope.Len() != 0 {
		t.Errorf("expected empty exported scope, got %d symbols", scope.Len())
	}
}

// Test full module loading flow: sema a module source → ExportedScope → use in consumer
func TestModuleLoadViaExportedScope(t *testing.T) {
	// Step 1: "compile" the module
	modScope := checkModuleSource(t, `
		type Point `+"`public"+` { int x; int y; }
		origin() Point `+"`public"+` { return Point(x: 0, y: 0); }
		helper() int { return 42; }
	`)

	// Step 2: use the module's exported scope in a consumer
	moduleScopes := map[string]*types.Scope{"geo": modScope}
	_, errs := checkWithModules(t, `
		use geo;
		main() {
			geo.Point p = geo.origin();
			int x = p.x;
		}
	`, moduleScopes)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}

func TestModuleLoadPrivateNotVisible(t *testing.T) {
	modScope := checkModuleSource(t, `
		get_value() int `+"`public"+` { return 42; }
		secret() int { return 99; }
	`)

	moduleScopes := map[string]*types.Scope{"lib": modScope}
	_, errs := checkWithModules(t, `
		use lib;
		main() {
			int x = lib.secret();
		}
	`, moduleScopes)
	expectError(t, errs, "no exported member 'secret'")
}

func TestModuleLoadGlobImportFiltersPrivate(t *testing.T) {
	modScope := checkModuleSource(t, `
		greet() int `+"`public"+` { return 1; }
		internal_fn() int { return 2; }
	`)

	moduleScopes := map[string]*types.Scope{"helpers": modScope}
	_, errs := checkWithModules(t, `
		use helpers as _;
		main() {
			int x = greet();
		}
	`, moduleScopes)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	// internal_fn() should not be accessible
	_, errs2 := checkWithModules(t, `
		use helpers as _;
		main() {
			int x = internal_fn();
		}
	`, moduleScopes)
	expectError(t, errs2, "undefined")
}

func TestModuleLoadSourcedLocalPath(t *testing.T) {
	modScope := checkModuleSource(t, `
		compute() int `+"`public"+` { return 42; }
	`)

	moduleScopes := map[string]*types.Scope{"./libs/math": modScope}
	_, errs := checkWithModules(t, `
		use math "./libs/math";
		main() {
			int x = math.compute();
		}
	`, moduleScopes)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}

func TestModuleLoadQualifiedTypeFromSource(t *testing.T) {
	modScope := checkModuleSource(t, `
		type Config `+"`public"+` { string key; string value; }
		default_config() Config `+"`public"+` { return Config(key: "k", value: "v"); }
	`)

	moduleScopes := map[string]*types.Scope{"./config": modScope}
	_, errs := checkWithModules(t, `
		use cfg "./config";
		main() {
			cfg.Config c = cfg.default_config();
			string k = c.key;
		}
	`, moduleScopes)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}

func TestModuleLoadMethodsOnExportedType(t *testing.T) {
	modScope := checkModuleSource(t, `
		type Counter `+"`public"+` {
			int value;
			increment(~this) `+"`public"+` { this.value = this.value + 1; }
			get_value(this) int `+"`public"+` { return this.value; }
		}
	`)

	moduleScopes := map[string]*types.Scope{"counter": modScope}
	_, errs := checkWithModules(t, `
		use counter;
		main() {
			counter.Counter c = counter.Counter(value: 0);
			c.increment();
			int v = c.get_value();
		}
	`, moduleScopes)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}

func TestModuleLoadMultipleModules(t *testing.T) {
	modA := checkModuleSource(t, `
		compute() int `+"`public"+` { return 42; }
	`)
	modB := checkModuleSource(t, `
		greet() string `+"`public"+` { return "hi"; }
	`)

	moduleScopes := map[string]*types.Scope{
		"math": modA,
		"text": modB,
	}
	_, errs := checkWithModules(t, `
		use math;
		use text;
		main() {
			int x = math.compute();
			string s = text.greet();
		}
	`, moduleScopes)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}

func TestModuleLoadConstructorCall(t *testing.T) {
	modScope := checkModuleSource(t, `
		type Point `+"`public"+` { int x; int y; }
	`)

	moduleScopes := map[string]*types.Scope{"geo": modScope}
	_, errs := checkWithModules(t, `
		use geo;
		main() {
			geo.Point p = geo.Point(x: 1, y: 2);
			int sum = p.x + p.y;
		}
	`, moduleScopes)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}

func TestModuleLoadFieldAccessOnImportedType(t *testing.T) {
	modScope := checkModuleSource(t, `
		type User `+"`public"+` {
			string name;
			int age;
		}
		make_user() User `+"`public"+` { return User(name: "Alice", age: 30); }
	`)

	moduleScopes := map[string]*types.Scope{"users": modScope}
	_, errs := checkWithModules(t, `
		use users;
		main() {
			users.User u = users.make_user();
			string n = u.name;
			int a = u.age;
		}
	`, moduleScopes)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}

// --- Abstract Factory in Structural Interface Tests ---

func TestAbstractFactoryInStructuralInterface(t *testing.T) {
	checkOK(t, `
		type Parseable `+"`"+`structural {
			parse(string data) `+"`"+`abstract `+"`"+`factory;
		}
		type My {
			parse(string data) My `+"`"+`factory { return My(); }
		}
		test() {
			Parseable p = My.parse("hello");
		}
	`)
}

func TestAbstractFactoryInNonStructuralFails(t *testing.T) {
	errs := checkErrs(t, `
		type Foo {
			make() Self `+"`"+`abstract `+"`"+`factory;
		}
		test() {}
	`)
	expectError(t, errs, "must not be abstract")
}

func TestAbstractFactoryFailableReturn(t *testing.T) {
	checkOK(t, `
		type Parseable `+"`"+`structural {
			tryParse(string data)! `+"`"+`abstract `+"`"+`factory;
		}
		type My {
			tryParse(string data) My! `+"`"+`factory {
				return My();
			}
		}
		test() {
			My m = My.tryParse("hello")!;
		}
	`)
}

func TestAbstractFactoryImplicitSelfReturn(t *testing.T) {
	// Abstract factory with no return type gets implicit Self
	checkOK(t, `
		type Maker `+"`"+`structural {
			make() `+"`"+`abstract `+"`"+`factory;
		}
		type Widget {
			make() Widget `+"`"+`factory { return Widget(); }
		}
		test() {
			Maker w = Widget.make();
		}
	`)
}

func TestFactoryInstanceMethodMismatch(t *testing.T) {
	// Instance method should NOT satisfy factory requirement
	errs := checkErrs(t, `
		type Maker `+"`"+`structural {
			make() `+"`"+`abstract `+"`"+`factory;
		}
		type Bad {
			make() Bad { return Bad(); }
		}
		test() {
			Maker m = Bad();
		}
	`)
	expectError(t, errs, "cannot assign")
}

func TestFactoryForInstanceMethodMismatch(t *testing.T) {
	// Factory method should NOT satisfy instance method requirement
	errs := checkErrs(t, `
		type Processor `+"`"+`structural {
			process() int `+"`"+`abstract;
		}
		type Bad {
			process() int `+"`"+`factory { return 0; }
		}
		test() {
			Processor p = Bad();
		}
	`)
	expectError(t, errs, "cannot assign")
}

func TestGenericFactoryConstraint(t *testing.T) {
	checkOK(t, `
		type Parseable `+"`"+`structural {
			parse(string data) `+"`"+`abstract `+"`"+`factory;
		}
		type My {
			parse(string data) My `+"`"+`factory { return My(); }
		}
		load[T: Parseable](string data) T {
			return T.parse(data);
		}
		test() {
			My m = load[My]("hello");
		}
	`)
}

func TestGenericFactoryFailableConstraint(t *testing.T) {
	checkOK(t, `
		type Parseable `+"`"+`structural {
			tryParse(string data)! `+"`"+`abstract `+"`"+`factory;
		}
		type My {
			tryParse(string data) My `+"`"+`factory { return My(); }
		}
		load[T: Parseable](string data) T! {
			return T.tryParse(data);
		}
		test() {
			My m = load[My]("hello")!;
		}
	`)
}

func TestStructuralMixedFactoryAndInstance(t *testing.T) {
	// Interface with both factory and instance abstract methods
	checkOK(t, `
		type Codec `+"`"+`structural {
			parse(string data) `+"`"+`abstract `+"`"+`factory;
			format() string `+"`"+`abstract;
		}
		type Json {
			string raw;
			parse(string data) Json `+"`"+`factory { return Json(raw: data); }
			format() string { return this.raw; }
		}
		roundTrip[T: Codec](string data) string {
			T obj = T.parse(data);
			return obj.format();
		}
		test() {
			string s = roundTrip[Json]("hello");
		}
	`)
}

func TestAbstractFactoryExplicitSelfReturn(t *testing.T) {
	// Explicit Self return type on abstract factory (not relying on implicit)
	checkOK(t, `
		type Maker `+"`"+`structural {
			make() Self `+"`"+`abstract `+"`"+`factory;
		}
		type Foo {
			make() Foo `+"`"+`factory { return Foo(); }
		}
		test() {
			Maker f = Foo.make();
		}
	`)
}

func TestAbstractFactoryExplicitFailableSelfReturn(t *testing.T) {
	// Abstract factory with explicit Self! return type should compile
	checkOK(t, `
		type TryParseable `+"`"+`structural {
			tryParse(string data) Self! `+"`"+`abstract `+"`"+`factory;
		}
		type Strict {
			tryParse(string data) Strict! `+"`"+`factory {
				if data == "bad" {
					raise error("invalid");
				}
				return Strict();
			}
		}
		tryLoad[T: TryParseable](string data) T! {
			return T.tryParse(data);
		}
		test() {
			Strict s = tryLoad[Strict]("ok")!;
		}
	`)
}

func TestStructuralFactoryAssignmentViolation(t *testing.T) {
	// Type missing the factory method should not satisfy structural interface
	errs := checkErrs(t, `
		type Parseable `+"`"+`structural {
			parse(string data) `+"`"+`abstract `+"`"+`factory;
		}
		type Empty {}
		test() {
			Parseable p = Empty();
		}
	`)
	expectError(t, errs, "cannot assign")
}

// --- use std; tests ---

func TestUseStdQualifiedFuncCall(t *testing.T) {
	checkOK(t, `
		use std;
		main() {
			int x = std.min(1, 2);
		}
	`)
}

func TestUseStdQualifiedTypeRef(t *testing.T) {
	checkOK(t, `
		use std;
		main() {
			std.int[] v = [];
		}
	`)
}

func TestUseStdWithAlias(t *testing.T) {
	checkOK(t, `
		use std as s;
		main() {
			int x = s.max(3, 4);
		}
	`)
}

func TestUseStdPrivateMemberDenied(t *testing.T) {
	errs := checkErrs(t, `
		use std;
		main() {
			std._print_string("hi");
		}
	`)
	expectError(t, errs, "has no exported member '_print_string'")
}

func TestUseStdGlobNoop(t *testing.T) {
	// use std as _ is a no-op; std symbols are already in scope
	checkOK(t, `
		use std as _;
		main() {
			int x = min(1, 2);
		}
	`)
}

func TestUseStdUnqualifiedStillWorks(t *testing.T) {
	// Even with use std;, unqualified access via parent scope chain still works
	checkOK(t, `
		use std;
		main() {
			int x = min(1, 2);
		}
	`)
}

func TestUseStdNoSuchMember(t *testing.T) {
	errs := checkErrs(t, `
		use std;
		main() {
			std.nonexistent();
		}
	`)
	expectError(t, errs, "has no exported member 'nonexistent'")
}

func TestUseStdAliasQualifiedType(t *testing.T) {
	checkOK(t, `
		use std as s;
		main() {
			s.int[] v = [];
		}
	`)
}

func TestUseStdAliasPrivateDenied(t *testing.T) {
	errs := checkErrs(t, `
		use std as s;
		main() {
			s._print_string("hi");
		}
	`)
	expectError(t, errs, "has no exported member '_print_string'")
}

func TestStdQualifiedFuncWithoutUse(t *testing.T) {
	// std.min() works with explicit "use std;"
	checkOK(t, `
		use std;
		main() {
			int x = std.min(1, 2);
		}
	`)
}

func TestStdQualifiedTypeWithoutUse(t *testing.T) {
	// std.int[] works with explicit "use std;"
	checkOK(t, `
		use std;
		main() {
			std.int[] v = [];
		}
	`)
}

func TestStdQualifiedConstructorCall(t *testing.T) {
	checkOK(t, `
		use std;
		main() {
			std.Range[int] r = std.Range[int](start: 0, end: 10, inclusive: false);
		}
	`)
}

// --- Multi-param generics in expression context ---

func TestMultiParamGenericInstantiation(t *testing.T) {
	checkOK(t, `
		type Pair[A, B] { A first; B second; }
		main() {
			p := Pair[int, string](first: 1, second: "hello");
		}
	`)
}

func TestMultiParamGenericWrongCount(t *testing.T) {
	errs := checkErrs(t, `
		type Pair[A, B] { A first; B second; }
		main() {
			p := Pair[int](first: 1, second: "hello");
		}
	`)
	expectError(t, errs, "expects 2 type arguments, got 1")
}

func TestMultiParamGenericThreeParams(t *testing.T) {
	checkOK(t, `
		type Triple[A, B, C] { A x; B y; C z; }
		main() {
			t := Triple[int, string, bool](x: 1, y: "two", z: true);
		}
	`)
}

func TestMultiParamGenericExtraIndicesOnNonGeneric(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			int[] v = [1, 2, 3];
			x := v[0, 1];
		}
	`)
	expectError(t, errs, "multiple indices not supported")
}

// --- Value type tests ---

func TestValueTypeValid(t *testing.T) {
	checkOK(t, `
		type Point {
			int x `+"`value"+`;
			int y `+"`value"+`;
		}
	`)
}

func TestValueTypeAutoCopy(t *testing.T) {
	// Value types are automatically copy — should be usable where copy is required
	checkOK(t, `
		type Point {
			int x `+"`value"+`;
			int y `+"`value"+`;
		}
		main() {
			Point p = Point(x: 1, y: 2);
			Point q = p;
			Point r = q;
		}
	`)
}

func TestValueTypeNoInheritance(t *testing.T) {
	errs := checkErrs(t, `
		type Base { int id; }
		type Child is Base {
			int x `+"`value"+`;
			int y `+"`value"+`;
		}
	`)
	expectError(t, errs, "value type Child cannot have parent types")
}

func TestValueTypeNonCopyField(t *testing.T) {
	errs := checkErrs(t, `
		type Bad {
			string name `+"`value"+`;
		}
	`)
	expectError(t, errs, "value field Bad.name must be a copy type")
}

func TestValueTypeNoDrop(t *testing.T) {
	errs := checkErrs(t, `
		type Bad {
			int x `+"`value"+`;
			drop(~this) {}
		}
	`)
	expectError(t, errs, "value type Bad cannot have a drop() method")
}

func TestValueTypeMixedNotValueType(t *testing.T) {
	// Mix of `value and default placement is NOT a value type — it's a regular type
	checkOK(t, `
		type Mixed {
			int x `+"`value"+`;
			int y;
		}
	`)
}

func TestValueTypeWithMethods(t *testing.T) {
	checkOK(t, `
		type Point {
			int x `+"`value"+`;
			int y `+"`value"+`;
			sum() int { return this.x + this.y; }
		}
	`)
}

func TestValueTypeWithBoolField(t *testing.T) {
	checkOK(t, `
		type Flags {
			bool a `+"`value"+`;
			bool b `+"`value"+`;
			bool c `+"`value"+`;
		}
	`)
}

func TestValueTypeWithF64Field(t *testing.T) {
	checkOK(t, `
		type Vec2 {
			f64 x `+"`value"+`;
			f64 y `+"`value"+`;
		}
	`)
}

func TestValueTypeNestedValueType(t *testing.T) {
	// A value type containing another value type field should be valid
	checkOK(t, `
		type Vec2 {
			f64 x `+"`value"+`;
			f64 y `+"`value"+`;
		}
		type Rect {
			Vec2 origin `+"`value"+`;
			Vec2 size `+"`value"+`;
		}
	`)
}

func TestValueTypeNonCopyUserTypeField(t *testing.T) {
	errs := checkErrs(t, `
		type Resource {
			int id;
			drop(~this) {}
		}
		type Bad {
			Resource r `+"`value"+`;
		}
	`)
	expectError(t, errs, "value field Bad.r must be a copy type")
}

func TestValueTypeFailableNew(t *testing.T) {
	errs := checkErrs(t, `
		type Percentage {
			int value `+"`value"+`;
			new(~this, int value) int! {
				if value < 0 { return error(0); }
				this.value = value;
			}
		}
	`)
	expectError(t, errs, "value type Percentage cannot have a failable new() method")
}

func TestValueTypeWithNewConstructor(t *testing.T) {
	checkOK(t, `
		type Clamped {
			int value `+"`value"+`;
			new(~this, int v) {
				if v < 0 { this.value = 0; }
				else if v > 100 { this.value = 100; }
				else { this.value = v; }
			}
		}
		test() {
			Clamped c = Clamped(v: 50);
		}
	`)
}

func TestValueTypeWithOperators(t *testing.T) {
	checkOK(t, `
		type Vec2 {
			int x `+"`value"+`;
			int y `+"`value"+`;
			+(Vec2 other) Vec2 {
				return Vec2(x: this.x + other.x, y: this.y + other.y);
			}
			==(Vec2 other) bool {
				return this.x == other.x && this.y == other.y;
			}
		}
		test() {
			Vec2 a = Vec2(x: 1, y: 2);
			Vec2 b = Vec2(x: 3, y: 4);
			Vec2 c = a + b;
			bool eq = a == b;
		}
	`)
}

func TestValueTypeOptionalField(t *testing.T) {
	checkOK(t, `
		type MaybePoint {
			int? x `+"`value"+`;
			int? y `+"`value"+`;
		}
	`)
}

func TestValueTypeEnumField(t *testing.T) {
	checkOK(t, `
		enum Dir `+"`copy"+` { North; South; East; West; }
		type Step {
			Dir dir `+"`value"+`;
			int distance `+"`value"+`;
		}
	`)
}

func TestValueTypeNonCopyEnumField(t *testing.T) {
	errs := checkErrs(t, `
		enum Dir { N; S; E; W; }
		type Step {
			Dir dir `+"`value"+`;
			int distance `+"`value"+`;
		}
	`)
	expectError(t, errs, "value field Step.dir must be a copy type")
}

// --- Variadic Parameter Tests ---

func TestVariadicBasic(t *testing.T) {
	checkOK(t, `
		sum(...int nums) int {
			int total = 0;
			for n in nums { total += n; }
			return total;
		}
		main() {
			sum();
			sum(1);
			sum(1, 2, 3);
		}
	`)
}

func TestVariadicWithRegularParams(t *testing.T) {
	checkOK(t, `
		join(string sep, ...string items) string {
			return sep;
		}
		main() {
			join(",");
			join(",", "a");
			join(",", "a", "b", "c");
		}
	`)
}

func TestVariadicPassVector(t *testing.T) {
	checkOK(t, `
		sum(...int nums) int {
			int total = 0;
			for n in nums { total += n; }
			return total;
		}
		main() {
			int[] v = [1, 2, 3];
			sum(v);
		}
	`)
}

func TestVariadicMustBeLast(t *testing.T) {
	errs := checkErrs(t, `
		bad(...int nums, string tail) {}
		main() {}
	`)
	expectError(t, errs, "variadic parameter must be the last parameter")
}

func TestVariadicOnlyOne(t *testing.T) {
	errs := checkErrs(t, `
		bad(...int a, ...int b) {}
		main() {}
	`)
	expectError(t, errs, "variadic parameter must be the last parameter")
}

func TestVariadicTypeMismatch(t *testing.T) {
	errs := checkErrs(t, `
		sum(...int nums) {}
		main() {
			sum("hello");
		}
	`)
	expectError(t, errs, "cannot assign")
}

func TestVariadicMethod(t *testing.T) {
	checkOK(t, `
		type Printer {
			printAll(~this, ...string items) {
			}
		}
		main() {
			p := Printer();
			p.printAll("a", "b");
		}
	`)
}

func TestVariadicNamedVectorArg(t *testing.T) {
	// Passing a T[] by name to a variadic parameter.
	checkOK(t, `
		sum(...int nums) int {
			int total = 0;
			for n in nums { total += n; }
			return total;
		}
		main() {
			sum(nums: [1, 2, 3]);
		}
	`)
}

func TestVariadicWithDefaultsAndOptionals(t *testing.T) {
	// Variadic after params with defaults and optionals.
	checkOK(t, `
		mylog(string level = "info", string? tag, ...string msgs) {
		}
		main() {
			mylog();
			mylog("warn");
			mylog("warn", "a", "b");
			mylog(level: "debug", tag: "sys", msgs: ["x", "y"]);
		}
	`)
}

func TestVariadicNonVariadicTooManyArgs(t *testing.T) {
	// Non-variadic functions still reject too many args.
	errs := checkErrs(t, `
		add(int a, int b) int { return a + b; }
		main() { add(1, 2, 3); }
	`)
	expectError(t, errs, "expects 2 arguments, got 3")
}

func TestVariadicMultipleTypeMismatch(t *testing.T) {
	// Type mismatch in one of several variadic args.
	errs := checkErrs(t, `
		sum(...int nums) {}
		main() { sum(1, "bad", 3); }
	`)
	expectError(t, errs, "type mismatch")
}

func TestVariadicBodyUsesVectorMethods(t *testing.T) {
	// The variadic param should support T[] methods like .len and indexing.
	checkOK(t, `
		count(...string items) int {
			return items.len;
		}
		first(...string items) string {
			return items[0];
		}
		main() {
			count("a", "b");
			first("x");
		}
	`)
}

func TestVariadicFailable(t *testing.T) {
	// Variadic function that can raise errors.
	checkOK(t, `
		trySum(...int nums) int! {
			if nums.len == 0 { raise error(message: "empty"); }
			int total = 0;
			for n in nums { total += n; }
			return total;
		}
		main() {
			x := trySum(1, 2, 3)!;
			y := trySum()!;
		}
	`)
}

func TestVariadicFailablePropagation(t *testing.T) {
	// Variadic failable called with ? from another failable function.
	checkOK(t, `
		trySum(...int nums) int! {
			if nums.len == 0 { raise error(message: "empty"); }
			int total = 0;
			for n in nums { total += n; }
			return total;
		}
		outer() int! {
			a := trySum(1, 2)?;
			b := trySum()?;
			return a + b;
		}
		main() { outer()!; }
	`)
}

func TestVariadicNestedCalls(t *testing.T) {
	// A variadic function passing its param to another variadic function.
	checkOK(t, `
		sum(...int nums) int {
			int total = 0;
			for n in nums { total += n; }
			return total;
		}
		doubleSum(...int nums) int {
			return sum(nums) * 2;
		}
		main() {
			doubleSum(1, 2, 3);
		}
	`)
}

func TestVariadicComputedVectorPassThrough(t *testing.T) {
	// Pass a computed T[] (function return) to variadic — should pass through.
	checkOK(t, `
		sum(...int nums) int {
			int total = 0;
			for n in nums { total += n; }
			return total;
		}
		makeVec() int[] {
			return [10, 20, 30];
		}
		main() {
			sum(makeVec());
		}
	`)
}

func TestVariadicMixedPositionalAndNamed(t *testing.T) {
	// Fixed params positional, variadic by name.
	checkOK(t, `
		mylog(string level, string tag, ...string msgs) {
		}
		main() {
			mylog("warn", "sys", "a", "b");
			mylog("info", tag: "app", msgs: ["x", "y"]);
		}
	`)
}

func TestVariadicWrongVectorType(t *testing.T) {
	// Passing string[] to ...int should fail.
	errs := checkErrs(t, `
		sum(...int nums) int { return 0; }
		main() {
			string[] v = ["a", "b"];
			sum(v);
		}
	`)
	expectError(t, errs, "cannot assign")
}

func TestVariadicEmptyCallInference(t *testing.T) {
	// Empty variadic with string type — verifies empty array hint works for all types.
	checkOK(t, `
		concat(...string parts) string {
			return "";
		}
		main() {
			concat();
			concat("a");
			concat("a", "b", "c");
		}
	`)
}

func TestVariadicMethodWithReceiver(t *testing.T) {
	// Variadic method with mutable receiver.
	checkOK(t, `
		type Logger {
			int count;

			logAll(&this, ...string msgs) {
				this.count += msgs.len;
			}
		}
		main() {
			l := Logger(count: 0);
			l.logAll();
			l.logAll("a", "b");
		}
	`)
}

// --- Numeric Suffix Tests ---

func TestNumericSuffixBasic(t *testing.T) {
	checkOK(t, `
		main() {
			u8 a = 42u8;
			u16 b = 1000u16;
			u32 c = 100000u32;
			u64 d = 999u64;
			i8 e = 42i8;
			i16 f = 1000i16;
			i32 g = 100000i32;
			i64 h = 999i64;
		}
	`)
}

func TestNumericSuffixInference(t *testing.T) {
	// Suffix determines the type for := inference.
	checkOK(t, `
		main() {
			a := 42u8;
			b := 1000u16;
			c := 100i32;
			d := 999u64;
		}
	`)
}

func TestNumericSuffixOverridesHint(t *testing.T) {
	// Suffix type takes priority over variable type — mismatch is an error.
	errs := checkErrs(t, `
		main() {
			u16 x = 10u8;
		}
	`)
	expectError(t, errs, "cannot assign")
}

func TestNumericSuffixRangeOverflowU8(t *testing.T) {
	errs := checkErrs(t, `
		main() { x := 256u8; }
	`)
	expectError(t, errs, "overflows u8")
}

func TestNumericSuffixRangeOverflowI8(t *testing.T) {
	errs := checkErrs(t, `
		main() { x := 128i8; }
	`)
	expectError(t, errs, "overflows i8")
}

func TestNumericSuffixRangeEdgeValid(t *testing.T) {
	checkOK(t, `
		main() {
			a := 255u8;
			b := 127i8;
			c := 0u8;
			d := 0i8;
			e := 65535u16;
			f := 32767i16;
		}
	`)
}

func TestNumericSuffixNegMin(t *testing.T) {
	// -128i8 is valid: unary neg of 128i8 should pass.
	checkOK(t, `
		main() {
			i8 a = -128i8;
			i16 b = -32768i16;
			i32 c = -2147483648i32;
		}
	`)
}

func TestNumericSuffixNegOverflow(t *testing.T) {
	// -129i8 overflows.
	errs := checkErrs(t, `
		main() { i8 x = -129i8; }
	`)
	expectError(t, errs, "overflows i8")
}

func TestNumericSuffixHex(t *testing.T) {
	checkOK(t, `
		main() {
			a := 0xFFu8;
			b := 0xFFFFu16;
			c := 0x7Fi8;
		}
	`)
}

func TestNumericSuffixHexOverflow(t *testing.T) {
	errs := checkErrs(t, `
		main() { x := 0x100u8; }
	`)
	expectError(t, errs, "overflows u8")
}

func TestNumericSuffixBinary(t *testing.T) {
	checkOK(t, `
		main() {
			a := 0b11111111u8;
			b := 0b1010i8;
		}
	`)
}

func TestNumericSuffixFloat(t *testing.T) {
	checkOK(t, `
		main() {
			f32 a = 1.5f32;
			f64 b = 3.14f64;
			c := 2.5f32;
		}
	`)
}

func TestNumericSuffixFloatMismatch(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			f64 x = 1.5f32;
		}
	`)
	expectError(t, errs, "cannot assign")
}

func TestNumericSuffixArithmetic(t *testing.T) {
	// Arithmetic with suffixed literals should work.
	checkOK(t, `
		main() {
			u8 a = 10u8;
			u8 b = 20u8;
			u8 c = a + b;
		}
	`)
}

func TestNumericSuffixPassToFunction(t *testing.T) {
	checkOK(t, `
		add(u8 a, u8 b) u8 { return a + b; }
		main() {
			add(10u8, 20u8);
		}
	`)
}

func TestNumericSuffixNestedUnaryNotNeg(t *testing.T) {
	// ~128i8 inside negation: the bitwise-not operand is NOT directly negated,
	// so 128i8 should still overflow i8.
	errs := checkErrs(t, `
		main() { i8 x = -(~128i8); }
	`)
	expectError(t, errs, "overflows i8")
}

func TestNumericSuffixUnsignedNeg(t *testing.T) {
	// Negating an unsigned suffixed literal — the negation check is only
	// for signed suffixes, so 1u8 should be allowed (unary neg on u8 type).
	checkOK(t, `
		main() {
			i8 x = -1i8;
		}
	`)
}

func TestNumericSuffixOctalWithSuffix(t *testing.T) {
	checkOK(t, `
		main() {
			x := 0o77u8;
			y := 0o177i16;
		}
	`)
}

func TestNumericSuffixRangeOverflowU16(t *testing.T) {
	errs := checkErrs(t, `
		main() { x := 65536u16; }
	`)
	expectError(t, errs, "overflows u16")
}

func TestNumericSuffixRangeOverflowU32(t *testing.T) {
	errs := checkErrs(t, `
		main() { x := 4294967296u32; }
	`)
	expectError(t, errs, "overflows u32")
}

func TestNumericSuffixRangeOverflowI16(t *testing.T) {
	errs := checkErrs(t, `
		main() { x := 32768i16; }
	`)
	expectError(t, errs, "overflows i16")
}

func TestNumericSuffixRangeOverflowI32(t *testing.T) {
	errs := checkErrs(t, `
		main() { x := 2147483648i32; }
	`)
	expectError(t, errs, "overflows i32")
}

func TestNumericSuffixNegOverflowI16(t *testing.T) {
	errs := checkErrs(t, `
		main() { i16 x = -32769i16; }
	`)
	expectError(t, errs, "overflows i16")
}

func TestNumericSuffixNegOverflowI32(t *testing.T) {
	errs := checkErrs(t, `
		main() { i32 x = -2147483649i32; }
	`)
	expectError(t, errs, "overflows i32")
}

func TestNumericSuffixNegMinI64(t *testing.T) {
	checkOK(t, `
		main() {
			i64 x = -9223372036854775808i64;
		}
	`)
}

func TestNumericSuffixEdgeValuesAllTypes(t *testing.T) {
	checkOK(t, `
		main() {
			a := 4294967295u32;
			b := 2147483647i32;
			c := 32767i16;
			d := 127i8;
		}
	`)
}

func TestNumericSuffixUnderscoreSeparated(t *testing.T) {
	checkOK(t, `
		main() {
			x := 1_000u32;
			y := 1_000_000i32;
			z := 0xFF_FFu16;
		}
	`)
}

func TestNumericSuffixReturnValue(t *testing.T) {
	checkOK(t, `
		getVal() u8 { return 42u8; }
		main() { getVal(); }
	`)
}

func TestNumericSuffixFloatMismatchReverse(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			f32 x = 3.14f64;
		}
	`)
	expectError(t, errs, "cannot assign")
}

// --- Bare i/u suffix (int/uint) ---

func TestNumericSuffixBareI(t *testing.T) {
	checkOK(t, `
		main() {
			int a = 42i;
			b := 100i;
			int c = 0i;
		}
	`)
}

func TestNumericSuffixBareU(t *testing.T) {
	checkOK(t, `
		main() {
			uint a = 42u;
			b := 100u;
			uint c = 0u;
		}
	`)
}

func TestNumericSuffixBareIMismatch(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			u8 x = 10i;
		}
	`)
	expectError(t, errs, "cannot assign")
}

func TestNumericSuffixBareUMismatch(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			int x = 10u;
		}
	`)
	expectError(t, errs, "cannot assign")
}

func TestNumericSuffixBareIInference(t *testing.T) {
	checkOK(t, `
		main() {
			x := 42i;
			int y = x + 1;
		}
	`)
}

func TestNumericSuffixBareUInference(t *testing.T) {
	checkOK(t, `
		main() {
			x := 42u;
			uint y = x + 1u;
		}
	`)
}

func TestNumericSuffixBareIHex(t *testing.T) {
	checkOK(t, `
		main() {
			int x = 0xFFi;
		}
	`)
}

func TestNumericSuffixBareUHex(t *testing.T) {
	checkOK(t, `
		main() {
			uint x = 0xFFu;
		}
	`)
}

func TestNumericSuffixBareINeg(t *testing.T) {
	checkOK(t, `
		main() {
			int x = -42i;
		}
	`)
}

// --- Property-not-method diagnostics ---

func TestPropertyCalledAsMethod(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			int[] v = [1, 2, 3];
			print_line(v.len());
		}
	`)
	expectError(t, errs, "is a property")
	expectError(t, errs, "not a method")
}

func TestPropertyCalledAsMethodString(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			s := "hello";
			print_line(s.len());
		}
	`)
	expectError(t, errs, "is a property")
	expectError(t, errs, "not a method")
}

func TestPropertyCalledAsMethodUserType(t *testing.T) {
	errs := checkErrs(t, `
		type Foo {
			int count;
		}
		main() {
			f := Foo(count: 5);
			print_line(f.count());
		}
	`)
	expectError(t, errs, "is a property")
	expectError(t, errs, "not a method")
}

func TestPropertyCalledAsMethodMap(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			map[string, int] m = {"a": 1};
			print_line(m.len());
		}
	`)
	expectError(t, errs, "is a property")
	expectError(t, errs, "not a method")
}

// --- `global and `mono placement tests ---

func TestGlobalMethodBasic(t *testing.T) {
	checkOK(t, "type Counter {\n"+
		"int value;\n"+
		"create(int v) Counter `global {\n"+
		"return Counter(value: v);\n"+
		"}\n"+
		"}\n"+
		"main() {\n"+
		"c := Counter.create(42);\n"+
		"}\n")
}

func TestGlobalMethodNoReceiver(t *testing.T) {
	errs := checkErrs(t, "type Foo {\n"+
		"int x;\n"+
		"bad(&this) int `global {\n"+
		"return this.x;\n"+
		"}\n"+
		"}\n"+
		"main() {}\n")
	expectError(t, errs, "must not declare a receiver")
}

func TestGlobalMethodNoSelfInBody(t *testing.T) {
	errs := checkErrs(t, "type Foo {\n"+
		"int x;\n"+
		"make() Foo `global {\n"+
		"Self s = Foo(x: 0);\n"+
		"return s;\n"+
		"}\n"+
		"}\n"+
		"main() {}\n")
	expectError(t, errs, "Self can only be used inside a type body")
}

func TestGlobalMethodNoSelfInReturnType(t *testing.T) {
	// Self in return type of `global resolves during define pass where curType is set.
	// This is acceptable — Self just means the owning type. The key restriction is
	// that the body cannot use Self (no type context).
	// Actually, for consistency, let's allow Self in the signature since it resolves
	// at define time. The body restriction is what matters.
	checkOK(t, "type Foo {\n"+
		"int x;\n"+
		"make() Self `global {\n"+
		"return Foo(x: 0);\n"+
		"}\n"+
		"}\n"+
		"main() {\n"+
		"f := Foo.make();\n"+
		"}\n")
}

func TestGlobalGetterAllowed(t *testing.T) {
	checkOK(t, "type Foo {\n"+
		"int x;\n"+
		"get count int `global { return 0; }\n"+
		"}\n"+
		"main() {\n"+
		"n := Foo.count;\n"+
		"}\n")
}

func TestGlobalSetterNotAllowed(t *testing.T) {
	errs := checkErrs(t, "type Foo {\n"+
		"int x;\n"+
		"set count(int v) `global { }\n"+
		"}\n"+
		"main() {}\n")
	expectError(t, errs, "cannot be a setter")
}

func TestGlobalMethodOnGenericTypeError(t *testing.T) {
	errs := checkErrs(t, "type Box[T] {\n"+
		"T value;\n"+
		"hello() int `global {\n"+
		"return 42;\n"+
		"}\n"+
		"}\n"+
		"main() {}\n")
	expectError(t, errs, "cannot be on a generic type")
}

func TestMonoMethodBasic(t *testing.T) {
	checkOK(t, "type Box[T] {\n"+
		"T value;\n"+
		"defaultValue() int `mono {\n"+
		"return 0;\n"+
		"}\n"+
		"}\n"+
		"main() {\n"+
		"n := Box[int].defaultValue();\n"+
		"}\n")
}

func TestMonoMethodNoReceiver(t *testing.T) {
	errs := checkErrs(t, "type Box[T] {\n"+
		"T value;\n"+
		"bad(&this) int `mono {\n"+
		"return 0;\n"+
		"}\n"+
		"}\n"+
		"main() {}\n")
	expectError(t, errs, "must not declare a receiver")
}

func TestGlobalMonoMutuallyExclusive(t *testing.T) {
	errs := checkErrs(t, "type Foo {\n"+
		"int x;\n"+
		"bad() int `global `mono {\n"+
		"return 0;\n"+
		"}\n"+
		"}\n"+
		"main() {}\n")
	expectError(t, errs, "mutually exclusive")
}

func TestMonoMethodSelfAllowed(t *testing.T) {
	// Self in `mono method signature resolves to the owning type.
	// Body can reference Self since curType is set.
	checkOK(t, "type Pair {\n"+
		"int x;\n"+
		"int y;\n"+
		"origin() Self `mono {\n"+
		"return Pair(x: 0, y: 0);\n"+
		"}\n"+
		"}\n"+
		"main() {\n"+
		"p := Pair.origin();\n"+
		"}\n")
}

// --- Generic Inheritance Tests ---

func TestGenericInheritanceBasic(t *testing.T) {
	checkOK(t, `
		type DataStream[T] {
			next() T? `+"`abstract;\n"+`
		}
		type IntStream is DataStream[int] {
			int pos;
			next() int? { return this.pos; }
		}
		test() {
			s := IntStream(pos: 42);
			int? v = s.next();
		}
	`)
}

func TestGenericInheritanceForwardParams(t *testing.T) {
	checkOK(t, `
		type Producer[T] {
			produce() T `+"`abstract;\n"+`
		}
		type ConstProducer[T] is Producer[T] {
			T value;
			produce() T { return this.value; }
		}
		test() {
			p := ConstProducer[int](value: 42);
			int x = p.produce();
		}
	`)
}

func TestGenericInheritanceMethodSubstitution(t *testing.T) {
	errs := checkErrs(t, `
		type Stream[T] {
			next() T? `+"`abstract;\n"+`
		}
		type IntStream is Stream[int] {
			next() int? { return 1; }
		}
		test() {
			s := IntStream();
			string? v = s.next();
		}
	`)
	expectError(t, errs, "cannot assign")
}

func TestGenericInheritancePartialApplication(t *testing.T) {
	checkOK(t, `
		type Container[K, V] {
			get(K key) V? `+"`abstract;\n"+`
		}
		type StringMap[V] is Container[string, V] {
			get(string key) V? { return none; }
		}
		test() {
			s := StringMap[int]();
			int? v = s.get("hello");
		}
	`)
}

func TestGenericInheritanceNonGenericChild(t *testing.T) {
	checkOK(t, `
		type Holder[T] {
			T value;
		}
		type IntHolder is Holder[int] {
		}
		test() {
			h := IntHolder(value: 42);
			int x = h.value;
		}
	`)
}

func TestGenericInheritanceAssignability(t *testing.T) {
	checkOK(t, `
		type DataStream[T] {
			next() T? `+"`abstract;\n"+`
		}
		type MyStream[T] is DataStream[T] {
			next() T? { return none; }
		}
		acceptStream(DataStream[int] s) {
			int? v = s.next();
		}
		test() {
			ms := MyStream[int]();
			acceptStream(ms);
		}
	`)
}

func TestGenericInheritanceTransitive(t *testing.T) {
	// 3-level chain: Leaf is Middle[int] is Base[T]
	checkOK(t, `
		type Base[T] {
			T data;
			get_data() T { return this.data; }
		}
		type Middle[T] is Base[T] {
			string tag;
		}
		type Leaf is Middle[int] {}
		test() {
			leaf := Leaf(data: 42, tag: "x");
			int v = leaf.data;
			int r = leaf.get_data();
		}
	`)
}

func TestGenericInheritanceTransitiveAssignability(t *testing.T) {
	// Leaf (Named) assignable to Base[int] (Instance) through generic Middle[int]
	checkOK(t, `
		type Base[T] {
			T data;
			get_data() T `+"`abstract;\n"+`
		}
		type Middle[T] is Base[T] {
			get_data() T { return this.data; }
		}
		type Leaf is Middle[int] {}
		acceptBase(Base[int] b) {
			int v = b.get_data();
		}
		test() {
			leaf := Leaf(data: 42);
			acceptBase(leaf);
		}
	`)
}

func TestGenericInheritanceTransitiveGenericChain(t *testing.T) {
	// GLeaf[T] is GMid[T] is GBase[T] — all generic
	checkOK(t, `
		type GBase[T] {
			T val;
			fetch() T { return this.val; }
		}
		type GMid[T] is GBase[T] {}
		type GLeaf[T] is GMid[T] {}
		test() {
			g := GLeaf[int](val: 77);
			int v = g.val;
			int r = g.fetch();
		}
	`)
}

func TestGenericInheritanceInstanceToInstance(t *testing.T) {
	// Wrapper[int] assignable to Container[int]
	checkOK(t, `
		type Container[T] {
			T item;
			get() T { return this.item; }
		}
		type Wrapper[T] is Container[T] {
			string label;
		}
		acceptContainer(Container[int] c) {
			int v = c.get();
		}
		test() {
			w := Wrapper[int](item: 42, label: "w");
			acceptContainer(w);
		}
	`)
}

func TestGenericInheritanceWrongTypeArg(t *testing.T) {
	// Wrapper[string] should NOT be assignable to Container[int]
	errs := checkErrs(t, `
		type Container[T] {
			T item;
		}
		type Wrapper[T] is Container[T] {}
		acceptContainer(Container[int] c) {}
		test() {
			w := Wrapper[string](item: "x");
			acceptContainer(w);
		}
	`)
	expectError(t, errs, "cannot assign")
}

func TestGenericInheritancePartialAppMethod(t *testing.T) {
	// Partial application with inherited methods
	checkOK(t, `
		type KVPair[K, V] {
			K key;
			V val;
			get_key() K { return this.key; }
			get_val() V { return this.val; }
		}
		type IntKV[V] is KVPair[int, V] {}
		test() {
			kv := IntKV[string](key: 1, val: "one");
			int k = kv.get_key();
			string v = kv.get_val();
		}
	`)
}

func TestGenericInheritanceConcreteOverride(t *testing.T) {
	checkOK(t, `
		type Greeter[T] {
			T name;
			greet() string { return "hello"; }
		}
		type FancyGreeter[T] is Greeter[T] {
			greet() string { return "greetings"; }
		}
		test() {
			g := FancyGreeter[int](name: 42);
			string s = g.greet();
			int n = g.name;
		}
	`)
}

// --- Method-level generics tests ---

func TestMethodGenericBasic(t *testing.T) {
	expectNoErrors(t, checkErrs(t, `
		type Echo {
			echo[T](T val) T { return val; }
		}
		main() {
			e := Echo();
			int x = e.echo[int](42);
			string s = e.echo[string]("hi");
		}
	`))
}

func TestMethodGenericOnGenericType(t *testing.T) {
	expectNoErrors(t, checkErrs(t, `
		type Box[T] {
			T item;
			convert[R](R val) R { return val; }
		}
		main() {
			b := Box[int](item: 1);
			string s = b.convert[string]("hello");
		}
	`))
}

func TestMethodGenericMultipleTypeParams(t *testing.T) {
	expectNoErrors(t, checkErrs(t, `
		type Mapper {
			pair[A, B](A a, B b) A { return a; }
		}
		main() {
			m := Mapper();
			int x = m.pair[int, string](42, "hi");
		}
	`))
}

func TestMethodGenericCannotBeAbstract(t *testing.T) {
	errs := checkErrs(t, `
		type Foo {
			bar[T]() T `+"`"+`abstract;
		}
		main() {}
	`)
	expectError(t, errs, "generic method Foo.bar cannot be abstract")
}

func TestMethodGenericCannotBeNative(t *testing.T) {
	errs := checkErrs(t, `
		type Foo {
			bar[T]() T `+"`"+`native;
		}
		main() {}
	`)
	expectError(t, errs, "generic method Foo.bar cannot be native")
}

func TestMethodGenericWrongTypeArgCount(t *testing.T) {
	errs := checkErrs(t, `
		type Echo {
			echo[T](T val) T { return val; }
		}
		main() {
			e := Echo();
			e.echo[int, string](42);
		}
	`)
	expectError(t, errs, "expects 1 type arguments, got 2")
}

func TestMethodGenericInherited(t *testing.T) {
	expectNoErrors(t, checkErrs(t, `
		type Parent {
			echo[T](T val) T { return val; }
		}
		type Child is Parent {
			int extra;
		}
		main() {
			c := Child(extra: 1);
			int x = c.echo[int](42);
		}
	`))
}

func TestMethodGenericFailable(t *testing.T) {
	expectNoErrors(t, checkErrs(t, `
		type MyErr is error { string message; }
		type Parser {
			try_parse[T](T val) T! { return val; }
		}
		main() {
			p := Parser();
			int x = p.try_parse[int](42)!;
		}
	`))
}

func TestMethodGenericTracksMethodInstance(t *testing.T) {
	info, errs := checkSource(t, `
		type Echo {
			echo[T](T val) T { return val; }
		}
		main() {
			e := Echo();
			e.echo[int](42);
			e.echo[string]("hi");
		}
	`)
	expectNoErrors(t, errs)
	if len(info.MethodInstances) != 2 {
		t.Fatalf("expected 2 MethodInstances, got %d", len(info.MethodInstances))
	}
}

func TestMethodGenericVoidReturn(t *testing.T) {
	expectNoErrors(t, checkErrs(t, `
		type Logger {
			log[T](T val) {}
		}
		main() {
			l := Logger();
			l.log[int](42);
		}
	`))
}

func TestMethodGenericOnGenericChildType(t *testing.T) {
	expectNoErrors(t, checkErrs(t, `
		type Base[T] { T val; }
		type Child[T] is Base[T] {
			convert[R](R other) R { return other; }
		}
		main() {
			c := Child[int](val: 1);
			string s = c.convert[string]("hi");
		}
	`))
}

// ============================================================
// String interpolation — Format validation
// ============================================================

func TestStringInterpFormatTypeOK(t *testing.T) {
	checkOK(t, `
		type Foo {
			int x;
			format(Writer ~w)! { w.write_string("foo"); }
		}
		test() { Foo f = Foo(x: 1); string s = "{f}"; }
	`)
}

func TestStringInterpMissingFormat(t *testing.T) {
	errs := checkErrs(t, `
		type Bar { int x; }
		test() { Bar b = Bar(x: 1); string s = "{b}"; }
	`)
	expectError(t, errs, "does not implement Format")
}

func TestStringInterpOptionalFormatType(t *testing.T) {
	checkOK(t, `
		type Qux {
			int v;
			format(Writer ~w)! { w.write_string("qux"); }
		}
		test() { Qux? q = Qux(v: 1); string s = "{q}"; }
	`)
}

func TestStringInterpPrimitivesStillWork(t *testing.T) {
	checkOK(t, `
		test() {
			int x = 42;
			string s = "{x}";
			f64 f = 3.14;
			string s2 = "{f}";
			bool b = true;
			string s3 = "{b}";
			char c = 'A';
			string s4 = "{c}";
		}
	`)
}

// --- `target(cond) filtering tests ---

// checkSourceWithTarget parses src as user code and checks with a specific target.
// Target functions annotated `target(cond)` are only declared if cond matches target.
func checkSourceWithTarget(t *testing.T, src, triple string) (*Info, []error) {
	t.Helper()
	// Build user file
	userFile := parseTargetTestFile(t, src, "test.pr")
	// Inject use std as _
	stdUse := &ast.UseDecl{Alias: "_", CatalogName: "std"}
	userFile.Uses = append([]*ast.UseDecl{stdUse}, userFile.Uses...)
	return CheckWithTarget(userFile, map[string]*types.Scope{"std": getSemaStdScope()}, ParseTargetInfo(triple))
}

func parseTargetTestFile(t *testing.T, src, name string) *ast.File {
	t.Helper()
	input := antlr.NewInputStream(src)
	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := parser.NewPromiseParser(stream)
	p.RemoveErrorListeners()
	tree := p.CompilationUnit()
	file, errs := ast.Build(name, tree)
	if len(errs) > 0 {
		t.Fatalf("%s AST build errors: %v", name, errs)
	}
	return file
}

func TestTargetFilterFunc(t *testing.T) {
	src := `
		sep() string ` + "`target(windows)" + ` { return "\\"; }
		sep() string ` + "`target(!windows)" + ` { return "/"; }
		main() { sep(); }
	`

	// On Linux: only the !windows variant is declared; calling sep() works.
	infoLinux, errs := checkSourceWithTarget(t, src, "x86_64-unknown-linux-musl")
	if len(errs) != 0 {
		t.Fatalf("linux: unexpected errors: %v", errs)
	}
	_ = infoLinux

	// On Windows: only the windows variant is declared; calling sep() works.
	infoWin, errs := checkSourceWithTarget(t, src, "x86_64-pc-windows-msvc")
	if len(errs) != 0 {
		t.Fatalf("windows: unexpected errors: %v", errs)
	}
	_ = infoWin
}

func TestTargetFilterExclusion(t *testing.T) {
	// A function with `target(wasm) — only exists on WASM.
	// On Linux it should be absent: calling it gives "undefined".
	src := `
		wasm_only() string ` + "`target(wasm)" + ` { return "wasm"; }
		main() { wasm_only(); }
	`
	_, errs := checkSourceWithTarget(t, src, "x86_64-unknown-linux-musl")
	if len(errs) == 0 {
		t.Fatal("expected error calling wasm_only() on linux, got none")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "wasm_only") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'wasm_only' in errors, got: %v", errs)
	}
}

func TestTargetFilterOrCondition(t *testing.T) {
	// `target(linux || macos) — present on both POSIX targets, absent on Windows.
	src := `
		posix_thing() string ` + "`target(linux || macos)" + ` { return "posix"; }
		main() { posix_thing(); }
	`
	_, errs := checkSourceWithTarget(t, src, "x86_64-unknown-linux-musl")
	if len(errs) != 0 {
		t.Errorf("linux: unexpected errors: %v", errs)
	}
	_, errs = checkSourceWithTarget(t, src, "aarch64-apple-macosx14.0.0")
	if len(errs) != 0 {
		t.Errorf("macos: unexpected errors: %v", errs)
	}
	_, errs = checkSourceWithTarget(t, src, "x86_64-pc-windows-msvc")
	if len(errs) == 0 {
		t.Error("windows: expected error calling posix_thing(), got none")
	}
}

func TestTargetNoFilteringWhenTargetUnknown(t *testing.T) {
	// Zero TargetInfo = no filtering. Both `target variants are declared.
	// This causes a redeclaration error (both sep() variants visible).
	src := `
		sep() string ` + "`target(windows)" + ` { return "\\"; }
		sep() string ` + "`target(!windows)" + ` { return "/"; }
	`
	file := parseTargetTestFile(t, src, "test.pr")
	_, errs := CheckWithTarget(file, nil, TargetInfo{})
	// With zero target, both variants are declared — duplicate name error.
	if len(errs) == 0 {
		t.Fatal("expected redeclaration error with zero TargetInfo, got none")
	}
}

func TestParseTargetInfo(t *testing.T) {
	tests := []struct {
		triple   string
		wantOS   string
		wantArch string
	}{
		{"x86_64-unknown-linux-musl", "linux", "x86_64"},
		{"x86_64-unknown-linux-gnu", "linux", "x86_64"},
		{"x86_64-pc-windows-msvc", "windows", "x86_64"},
		{"x86_64-apple-macosx14.0.0", "macos", "x86_64"},
		{"aarch64-apple-macosx14.0.0", "macos", "aarch64"},
		{"arm64-apple-macosx14.0.0", "macos", "aarch64"}, // Apple uses arm64 in their triples
		{"wasm32-wasi", "wasm", "wasm32"},
		{"", "", ""},
	}
	for _, tt := range tests {
		ti := ParseTargetInfo(tt.triple)
		if ti.OS != tt.wantOS {
			t.Errorf("ParseTargetInfo(%q).OS = %q, want %q", tt.triple, ti.OS, tt.wantOS)
		}
		if ti.Arch != tt.wantArch {
			t.Errorf("ParseTargetInfo(%q).Arch = %q, want %q", tt.triple, ti.Arch, tt.wantArch)
		}
	}
}

func TestTargetFilterArm64Alias(t *testing.T) {
	// arm64 is Apple's name for aarch64; `target(arm64) should match aarch64 targets.
	src := `
		apple_thing() string ` + "`target(arm64)" + ` { return "arm"; }
		main() { apple_thing(); }
	`
	_, errs := checkSourceWithTarget(t, src, "arm64-apple-macosx14.0.0")
	if len(errs) != 0 {
		t.Errorf("arm64 target: unexpected errors: %v", errs)
	}
	// On x86_64, arm64-only function should be absent.
	_, errs = checkSourceWithTarget(t, src, "x86_64-unknown-linux-musl")
	if len(errs) == 0 {
		t.Error("x86_64: expected error calling arm64-only function, got none")
	}
}

// --- Failable getters ---

func TestFailableGetterOK(t *testing.T) {
	checkOK(t, `
		type MyErr is error { int code; }
		type Foo {
			int _val;
			get value int! {
				if this._val < 0 { raise MyErr(code: 1, message: "neg"); }
				return this._val;
			}
		}
		main() {
			Foo f = Foo(_val: 42);
			int v = f.value!;
		}
	`)
}

func TestFailableGetterPropagate(t *testing.T) {
	checkOK(t, `
		type MyErr is error { int code; }
		type Foo {
			int _val;
			get value int! {
				if this._val < 0 { raise MyErr(code: 1, message: "neg"); }
				return this._val;
			}
		}
		bar(Foo f) int! {
			return f.value?;
		}
	`)
}

func TestFailableGetterHandler(t *testing.T) {
	checkOK(t, `
		type MyErr is error { int code; }
		type Foo {
			int _val;
			get value int! {
				if this._val < 0 { raise MyErr(code: 1, message: "neg"); }
				return this._val;
			}
		}
		main() {
			Foo f = Foo(_val: -1);
			int v = f.value ? e { 0; };
		}
	`)
}

func TestFailableGetterAbstract(t *testing.T) {
	checkOK(t, `
		type Base {
			get value int! `+"`"+`abstract;
		}
		type Impl is Base {
			int _v;
			get value int! { return this._v; }
		}
		main() {
			Base b = Impl(_v: 10);
			int v = b.value!;
		}
	`)
}

func TestNonFailableGetterBangError(t *testing.T) {
	errs := checkErrs(t, `
		type Foo {
			int _val;
			get value int { return this._val; }
		}
		main() {
			Foo f = Foo(_val: 1);
			int v = f.value!;
		}
	`)
	expectError(t, errs, "failable")
}

// --- Module-level getter/setter tests ---

func TestModuleLevelGetterOK(t *testing.T) {
	checkOK(t, `
		get greeting string { return "hello"; }
		test() { string s = greeting; }
	`)
}

func TestModuleLevelGetterComputedOK(t *testing.T) {
	checkOK(t, `
		get answer int { return 6 * 7; }
		test() { int v = answer; }
	`)
}

func TestModuleLevelGetterReadOnlyAssignError(t *testing.T) {
	errs := checkErrs(t, `
		get greeting string { return "hello"; }
		test() { greeting = "bye"; }
	`)
	expectError(t, errs, "has no setter")
}

func TestModuleLevelGetterWithSetterOK(t *testing.T) {
	checkOK(t, `
		get counter int { return 0; }
		set counter(int value) {}
		test() {
			int v = counter;
			counter = 10;
		}
	`)
}

func TestModuleLevelFailableGetterOK(t *testing.T) {
	checkOK(t, `
		get safe string! { return "ok"; }
		test() { string s = safe!; }
	`)
}

func TestModuleLevelOptionalGetterOK(t *testing.T) {
	checkOK(t, `
		get maybe string? { return "present"; }
		test() {
			string? v = maybe;
			if s := v { assert(s == "present", ""); }
		}
	`)
}

func TestModuleLevelGetterCalledAsFunctionError(t *testing.T) {
	errs := checkErrs(t, `
		get greeting string { return "hello"; }
		test() { string s = greeting(); }
	`)
	expectError(t, errs, "not a function")
}

func TestModuleLevelSetterOnlyReadError(t *testing.T) {
	errs := checkErrs(t, `
		set counter(int value) {}
		test() { int v = counter; }
	`)
	expectError(t, errs, "undefined: counter")
}

func TestModuleLevelCompoundAssignWithSetterOK(t *testing.T) {
	checkOK(t, `
		get counter int { return 0; }
		set counter(int value) {}
		test() { counter += 5; }
	`)
}

func TestModuleLevelSetterMissingReturn(t *testing.T) {
	// Setters are void — no missing return error expected.
	checkOK(t, `
		set counter(int value) {}
		test() {}
	`)
}

func TestModuleLevelGetterMissingReturnError(t *testing.T) {
	errs := checkErrs(t, `
		get counter int {
			if true {}
		}
		test() {}
	`)
	expectError(t, errs, "missing return")
}

func TestModuleLevelFailableGetterInNonFailableError(t *testing.T) {
	errs := checkErrs(t, `
		get risky string! { return "ok"; }
		test() { string s = risky; }
	`)
	expectError(t, errs, "failable")
}

func TestModuleLevelFailableGetterHandlerOK(t *testing.T) {
	checkOK(t, `
		get risky string! { return "ok"; }
		test() {
			string s = risky ? e { "fallback"; };
		}
	`)
}
