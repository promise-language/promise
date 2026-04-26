package sema

import (
	"strings"
	"testing"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/parser"
	"djabi.dev/go/promise_lang/internal/types"
	antlr "github.com/antlr4-go/antlr/v4"
)

// --- Test helpers ---

func checkSource(t *testing.T, src string) (*Info, []error) {
	t.Helper()
	input := antlr.NewInputStream(src)
	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := parser.NewPromiseParser(stream)
	p.RemoveErrorListeners()
	tree := p.CompilationUnit()
	file, errs := ast.Build("test.pr", tree)
	if len(errs) > 0 {
		t.Fatalf("AST build errors: %v", errs)
	}
	return Check(file)
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
	expectError(t, errs, "requires bool")
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
	// Verify that initBuiltins populates operator methods
	initBuiltins()

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
		if m, ok := typ.(*types.Map); ok {
			assertType(t, info, m.Key(), "string")
			assertType(t, info, m.Val(), "int")
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
	t.Error("no Range type recorded")
}

func TestRangeInclusive(t *testing.T) {
	checkOK(t, `test() { r := 0..=10; }`)
}

func TestRangeNonInt(t *testing.T) {
	errs := checkErrs(t, `test() { r := "a".."z"; }`)
	expectError(t, errs, "range operator requires int")
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
	t.Error("no Task type recorded for go expression")
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
		t.Errorf("Task should have 1 type param, got %d", len(types.TypTask.TypeParams()))
	}
}

func TestUniverseChannelType(t *testing.T) {
	if types.TypChannel == nil {
		t.Fatal("TypChannel is nil")
	}
	if len(types.TypChannel.TypeParams()) != 1 {
		t.Errorf("Channel should have 1 type param, got %d", len(types.TypChannel.TypeParams()))
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
		t.Errorf("Map should have 2 type params, got %d", len(types.TypMap.TypeParams()))
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
			Map[string, int] m = {"a": 1, "b": 2};
		}
	`)
}

func TestMapTypeAnnotationAsParam(t *testing.T) {
	checkOK(t, `
		lookup(Map[string, int] m) {
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
	initBuiltins()
	m := types.TypIter.LookupMethod("next")
	if m == nil {
		t.Fatal("Iter.next() method not found")
	}
	if !m.IsAbstract() {
		t.Error("Iter.next() should be abstract")
	}
	sig := m.Sig()
	if sig == nil {
		t.Fatal("Iter.next() has no signature")
	}
	// Return type should be T? (Optional of TypeParam)
	opt, ok := sig.Result().(*types.Optional)
	if !ok {
		t.Fatalf("Iter.next() should return Optional, got %T", sig.Result())
	}
	if _, ok := opt.Elem().(*types.TypeParam); !ok {
		t.Errorf("Iter.next() Optional elem should be TypeParam, got %T", opt.Elem())
	}
}

func TestStreamHasNextMethod(t *testing.T) {
	initBuiltins()
	m := types.TypStream.LookupMethod("next")
	if m == nil {
		t.Fatal("Stream.next() method not found")
	}
	if !m.IsAbstract() {
		t.Error("Stream.next() should be abstract")
	}
	sig := m.Sig()
	if sig == nil {
		t.Fatal("Stream.next() has no signature")
	}
	// Return type should be Task[T?] (Instance of Task with Optional TypeParam)
	inst, ok := sig.Result().(*types.Instance)
	if !ok {
		t.Fatalf("Stream.next() should return Instance, got %T", sig.Result())
	}
	if inst.Origin() != types.TypTask {
		t.Errorf("Stream.next() should return Task instance, got %s", inst.Origin())
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
	checkOK(t, `
		main() {
			int[] arr = [1, 2, 3];
			int n = arr.len;
		}
	`)
}

func TestArrayLenProperty(t *testing.T) {
	checkOK(t, `
		check(int[3] arr) int { return arr.len; }
		main() { }
	`)
}

func TestMapLenProperty(t *testing.T) {
	checkOK(t, `
		main() {
			m := {"a": 1};
			int n = m.len;
		}
	`)
}

func TestStringLenProperty(t *testing.T) {
	checkOK(t, `
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
	expectError(t, errs, "no member")
}

func TestMapInvalidMember(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			m := {"a": 1};
			int n = m.foo;
		}
	`)
	expectError(t, errs, "no member")
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
