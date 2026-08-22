package codegen

import (
	"strings"
	"testing"

	antlr "github.com/antlr4-go/antlr/v4"
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/parser"
	"github.com/promise-language/promise/compiler/internal/sema"
)

func TestPanicBody(t *testing.T) {
	ir := generateIR(t, `
		main() {}
	`)
	// B0228: promise_panic now sets TLS flag and returns (no longjmp/exit)
	assertContains(t, ir, "define void @promise_panic(i8*")
	assertContains(t, ir, "store i8 1, i8* @__promise_panic_flag")     // set panic flag
	assertContains(t, ir, "store i8* %msg, i8** @__promise_panic_msg") // store msg
	assertContains(t, ir, "store i8 1, i8* @__promise_panic_type")     // type=1 (.rodata)
	assertContains(t, ir, "fatal: panic during panic recovery")        // double-panic message
	assertContains(t, ir, "call void @pal_exit(i32 134)")              // double-panic exit
}

func TestPanicMsgBody(t *testing.T) {
	ir := generateIR(t, `
		panic_msg(string msg) `+"`"+`extern("promise_panic_msg");
		main() { panic_msg("boom"); }
	`)
	assertContains(t, ir, "define void @promise_panic_msg(i8*")
	assertContains(t, ir, "bitcast i8* %msg to %promise_string_v*")
	// B0228: promise_panic_msg now sets TLS flag and returns (no longjmp/exit)
	assertContains(t, ir, "store i8 1, i8* @__promise_panic_flag") // set panic flag
	assertContains(t, ir, "store i8* %")                           // store C string msg
	assertContains(t, ir, "store i8 2, i8* @__promise_panic_type") // type=2 (heap)
	assertContains(t, ir, "call i8* @pal_alloc(")                  // allocate C string copy
	assertContains(t, ir, "fatal: panic during panic recovery")    // double-panic message
}

func TestFailableNewConstructorCodegen(t *testing.T) {
	ir := generateIR(t, `
		type Port {
			int value;
			new!(~this, int value) void {
				if value < 1 {
					raise error(message: "invalid port");
				}
				this.value = value;
			}
		}
		main!() {
			Port p = Port(value: 80)?!;
		}
	`)
	// Failable new returns a result type { i1, i8* }
	assertContains(t, ir, "define { i1, i8* } @Port.new(i8* %this")
	// Constructor call should call new and check the error
	assertContains(t, ir, "call { i1, i8* } @Port.new(")
}

func TestGenericFailableFactoryPassthrough(t *testing.T) {
	ir := generateIR(t, `
		type TryParseable `+"`"+`structural {
			tryParse!(string data) `+"`"+`abstract `+"`"+`factory;
		}
		type Strict {
			tryParse!(string data) Strict `+"`"+`factory {
				if data == "bad" {
					raise error("invalid");
				}
				return Strict();
			}
		}
		tryLoad![T: TryParseable](string data) T {
			return T.tryParse(data);
		}
		main() {
			Strict s = tryLoad[Strict]("ok")?!;
		}
	`)
	// Monomorphized tryLoad[Strict] should call Strict.tryParse directly
	assertContains(t, ir, "call { i1, { i8*, i8* }, i8* } @Strict.tryParse(")
	// Failable passthrough: tryLoad[Strict] should return the result directly
	// (single ret of the call result, no insertvalue wrapping)
	assertContains(t, ir, "@\"tryLoad[Strict]\"(")
	assertContains(t, ir, "ret { i1, { i8*, i8* }, i8* } %")
}

func TestFailableDeclaration(t *testing.T) {
	ir := generateIR(t, `
		parse!(string s) int { return 0; }
		main() { }
	`)
	// Return type should be result struct { i1, i64, i8* }
	assertContains(t, ir, "define { i1, i64, i8* } @__user.parse(i8* %s)")
}

func TestReturnInFailable(t *testing.T) {
	ir := generateIR(t, `
		parse!(string s) int { return 42; }
		main() { }
	`)
	// Should wrap value in Ok result: tag=false, value, null error
	assertContains(t, ir, "insertvalue { i1, i64, i8* }")
	assertContains(t, ir, "i1 false")
	assertContains(t, ir, "ret { i1, i64, i8* }")
}

func TestFailableVoidBangShorthand(t *testing.T) {
	ir := generateIR(t, `
		fail!() { raise error(message: "oops"); }
		main() { }
	`)
	// Should produce void result struct { i1, i8* }
	assertContains(t, ir, "define { i1, i8* } @__user.fail()")
}

func TestFailableMain(t *testing.T) {
	ir := generateIR(t, `
		main!() {
			raise error(message: "boom");
		}
	`)
	// Body compiled into helper function
	assertContains(t, ir, "define { i1, i8* } @__promise_main_body()")
	// Error path panics
	assertContains(t, ir, "unhandled error in main")
}

func TestRaiseStmt(t *testing.T) {
	ir := generateIR(t, `
		parse!(string s) int { raise error(message: "parse error"); }
		main() { }
	`)
	// Should wrap error in Error result: tag=true
	assertContains(t, ir, "i1 true")
	assertContains(t, ir, "ret { i1, i64, i8* }")
	// Should create the error message string
	assertContains(t, ir, `c"parse error"`)
	// Should extract instance pointer from value struct
	assertContains(t, ir, "extractvalue { i8*, i8* }")
}

func TestErrorPropagate(t *testing.T) {
	ir := generateIR(t, `
		parse!(string s) int { return 0; }
		process!() int {
			x := parse("42")?^;
			return x;
		}
		main() { }
	`)
	// Should have propagation and ok blocks
	assertContains(t, ir, "error.propagate")
	assertContains(t, ir, "error.ok")
	// Should extract tag from result
	assertContains(t, ir, "extractvalue { i1, i64, i8* }")
}

func TestErrorUnwrap(t *testing.T) {
	ir := generateIR(t, `
		parse!(string s) int { return 0; }
		main() {
			x := parse("42")?!;
		}
	`)
	// Should have panic and ok blocks
	assertContains(t, ir, "error.panic")
	assertContains(t, ir, "error.ok")
	// B0200: Should extract message string from error instance before panicking.
	// The error.panic block must bitcast the error instance to load the message
	// field, then create a C string copy for promise_panic.
	assertContains(t, ir, "call void @promise_panic(")
	assertContains(t, ir, "unreachable")
	// Verify message extraction: bitcast to error instance type, GEP to message field
	assertContains(t, ir, "getelementptr %promise_error_i")
}

// B0256: emitErrorPanic heap-allocates a C string copy but promise_panic sets
// type=1 (.rodata). The fix overwrites panic_type to 2 (heap) after the call
// so goroutine_exit frees it. T0142: now calls promise_panic_at which handles
// the type=2 store internally.
func TestErrorPanicSetsHeapType(t *testing.T) {
	ir := generateIR(t, `
		parse!(string s) int { return 0; }
		main() {
			x := parse("42")?!;
		}
	`)
	// emitErrorPanic calls promise_panic_at (T0142)
	assertContains(t, ir, "call void @promise_panic_at(")
	// promise_panic_at body stores type=2 after calling promise_panic
	assertContains(t, ir, "store i8 2, i8* @__promise_panic_type")
}

// T0142: error panic via ?! includes source file and line number.
func TestErrorPanicSourceLocation(t *testing.T) {
	ir := generateIR(t, `
		fail!() int { return 0; }
		main() {
			x := fail()?!;
		}
	`)
	// Should call promise_panic_at with a filename global and line number constant
	assertContains(t, ir, "call void @promise_panic_at(")
	// Filename global should be a .file. prefixed constant
	assertContains(t, ir, "@.file.")
	// promise_panic_at should be defined with its body (not just declared)
	assertContains(t, ir, "define void @promise_panic_at(")
}

// T0125: When func()?! returns a string, the unwrapped i8* must be tracked
// as a stmt temp so it gets freed at statement end if not claimed.
func TestErrorUnwrapStringTemp(t *testing.T) {
	ir := generateIR(t, `
		make_str!() string { return "hello"; }
		main() {
			int n = make_str()?!.len;
		}
	`)
	// Should have string temp tracking: store to alloca + drop flag
	assertContains(t, ir, "tmp.drop")
	assertContains(t, ir, "promise_string_drop")
}

// B0260: When func()?^ propagates a string result, the ok-path i8* must be
// tracked as a stmt temp so it gets freed if not claimed (e.g., by vec.push
// which dups the string).
func TestErrorPropagateStringTemp(t *testing.T) {
	ir := generateIR(t, `
		make_str!() string { return "hello"; }
		wrap!(string[] move v) string[] {
			v.push(make_str()?^);
			return v;
		}
	`)
	// Should have string temp tracking for the propagated string result
	assertContains(t, ir, "error.propagate")
	assertContains(t, ir, "error.ok")
	// The decoded string must be tracked as a temp and dropped after push
	assertContains(t, ir, "tmp.drop")
	assertContains(t, ir, "promise_string_drop")
}

func TestErrorHandler(t *testing.T) {
	ir := generateIR(t, `
		parse!(string s) int { return 0; }
		main() {
			x := parse("42") ? e { 0; };
		}
	`)
	// Should have handler, ok, and merge blocks
	assertContains(t, ir, "error.handler")
	assertContains(t, ir, "error.ok")
	assertContains(t, ir, "error.merge")
}

func TestErrorHandlerDiscard(t *testing.T) {
	ir := generateIR(t, `
		parse!(string s) int { return 0; }
		main() {
			x := parse("42") ? _ { 0; };
		}
	`)
	assertContains(t, ir, "error.handler")
	assertContains(t, ir, "error.ok")
}

func TestVoidFailable(t *testing.T) {
	ir := generateIR(t, `
		validate!(string s) void { return; }
		main() { }
	`)
	// Return type should be { i1, i8* }
	assertContains(t, ir, "define { i1, i8* } @__user.validate(i8* %s)")
}

func TestVoidRaise(t *testing.T) {
	ir := generateIR(t, `
		validate!(string s) void { raise error(message: "invalid"); }
		main() { }
	`)
	assertContains(t, ir, "i1 true")
	assertContains(t, ir, "ret { i1, i8* }")
}

func TestFailableMethod(t *testing.T) {
	ir := generateIR(t, `
		type Parser {
			string input;
			parse!(this) int {
				return 42;
			}
		}
		main() { }
	`)
	assertContains(t, ir, "define { i1, i64, i8* } @Parser.parse(i8* %this)")
}

func TestFailableAutoTerminator(t *testing.T) {
	ir := generateIR(t, `
		validate!(string s) void {
			if true {
				return;
			}
		}
		main() { }
	`)
	// Auto-terminator on fall-through path should wrap in Ok (tag=false)
	assertContains(t, ir, "i1 false")
	assertContains(t, ir, "ret { i1, i8* }")
}

func TestVoidFailablePropagate(t *testing.T) {
	ir := generateIR(t, `
		validate!(string s) void { raise error(message: "invalid"); }
		process!() void {
			validate("x")?^;
		}
		main() { }
	`)
	// Should propagate error from void failable callee
	assertContains(t, ir, "error.propagate")
	assertContains(t, ir, "error.ok")
	// Callee returns { i1, i8* }, caller also returns { i1, i8* }
	assertContains(t, ir, "extractvalue { i1, i8* }")
}

func TestVoidFailableUnwrap(t *testing.T) {
	ir := generateIR(t, `
		validate!(string s) void { raise error(message: "invalid"); }
		main() {
			validate("x")?!;
		}
	`)
	assertContains(t, ir, "error.panic")
	assertContains(t, ir, "error.ok")
	assertContains(t, ir, "call void @promise_panic(")
}

func TestVoidFailableHandler(t *testing.T) {
	ir := generateIR(t, `
		validate!(string s) void { raise error(message: "invalid"); }
		main() {
			validate("x") ? e { };
		}
	`)
	assertContains(t, ir, "error.handler")
	assertContains(t, ir, "error.ok")
	assertContains(t, ir, "error.merge")
}

func TestNestedErrorPropagation(t *testing.T) {
	ir := generateIR(t, `
		a!() int { return 1; }
		b!() int { return a()?^; }
		c!() int { return b()?^; }
		main() { }
	`)
	// Both b and c should have propagation blocks
	assertContains(t, ir, "error.propagate")
	assertContains(t, ir, "error.ok")
}

func TestErrorHandlerWithReturn(t *testing.T) {
	ir := generateIR(t, `
		parse!(string s) int { return 0; }
		process(string s) int {
			x := parse(s) ? e { return -1; };
			return x;
		}
		main() { }
	`)
	// Handler block should contain a return (terminator)
	assertContains(t, ir, "error.handler")
	assertContains(t, ir, "error.ok")
}

func TestFailableConditionalRaiseReturn(t *testing.T) {
	ir := generateIR(t, `
		parse!(string s) int {
			if s == "" {
				raise error(message: "empty");
			}
			return 42;
		}
		main() { }
	`)
	// Should have both Ok and Error paths
	assertContains(t, ir, "i1 true")
	assertContains(t, ir, "i1 false")
	assertContains(t, ir, "ret { i1, i64, i8* }")
}

// --- Typed Error Handler Tests ---

func TestTypedErrorHandler(t *testing.T) {
	ir := generateIR(t, `
		type IoError is error {
			int code;
		}
		fail!() void { raise IoError(message: "disk full", code: 28); }
		process!() void {
			fail() ? e is IoError { };
		}
		main() { }
	`)
	// Should have RTTI type check
	assertContains(t, ir, "call i32 @promise_type_is(")
	// Should have typed match/nomatch blocks
	assertContains(t, ir, "error.typed.match")
	assertContains(t, ir, "error.typed.nomatch")
}

func TestTypedErrorHandlerInFailable(t *testing.T) {
	ir := generateIR(t, `
		type IoError is error {
			int code;
		}
		fail!() void { raise IoError(message: "disk full", code: 28); }
		process!() void {
			fail() ? e is IoError { };
		}
		main() { }
	`)
	// Nomatch path in failable function should propagate error (ret)
	assertContains(t, ir, "error.typed.nomatch")
	assertContains(t, ir, "ret { i1, i8* }")
}

func TestTypedErrorHandlerNomatchPropagates(t *testing.T) {
	ir := generateIR(t, `
		type IoError is error {
			int code;
		}
		fail!() void { raise IoError(message: "disk full", code: 28); }
		process!() void {
			fail() ? e is IoError { };
		}
		main() { }
	`)
	// Nomatch path in failable function should propagate error
	assertContains(t, ir, "error.typed.nomatch")
	assertContains(t, ir, "ret { i1, i8* }")
}

func TestTypedErrorHandlerDiscardBinding(t *testing.T) {
	ir := generateIR(t, `
		type IoError is error {
			int code;
		}
		fail!() void { raise IoError(message: "disk full", code: 28); }
		process!() void {
			fail() ? _ is IoError { };
		}
		main() { }
	`)
	assertContains(t, ir, "call i32 @promise_type_is(")
	assertContains(t, ir, "error.typed.match")
}

func TestTypedErrorHandlerElse(t *testing.T) {
	ir := generateIR(t, `
		type IoError is error { int code; }
		fail!() void { raise IoError(message: "disk full", code: 28); }
		process() {
			fail() ? e is IoError { } else { };
		}
		main() { }
	`)
	assertContains(t, ir, "error.typed.match")
	assertContains(t, ir, "error.typed.nomatch")
	// No panic in nomatch — else handles it
	assertNotContains(t, ir, "unhandled error type")
}

func TestTypedErrorHandlerElseWithBinding(t *testing.T) {
	ir := generateIR(t, `
		type IoError is error { int code; }
		fail!() void { raise IoError(message: "disk full", code: 28); }
		get_msg() string {
			fail() ? e is IoError { return "io"; } else e { return e.message; };
			return "";
		}
		main() { }
	`)
	assertContains(t, ir, "error.typed.match")
	assertContains(t, ir, "error.typed.nomatch")
}

func TestTypedErrorHandlerBang(t *testing.T) {
	ir := generateIR(t, `
		type IoError is error { int code; }
		fail!() void { raise IoError(message: "disk full", code: 28); }
		process() {
			fail() ? e is IoError { }!;
		}
		main() { }
	`)
	assertContains(t, ir, "error.typed.match")
	assertContains(t, ir, "error.typed.nomatch")
	// Nomatch panics via promise_panic
	assertContains(t, ir, "call void @promise_panic(")
	assertContains(t, ir, "unreachable")
}

func TestUntypedErrorHandlerUnchanged(t *testing.T) {
	ir := generateIR(t, `
		fail!() void { raise error(message: "oops"); }
		main() {
			fail() ? e { };
		}
	`)
	// Untyped handler should NOT have typed match/nomatch blocks
	assertContains(t, ir, "error.handler")
	assertNotContains(t, ir, "error.typed.match")
	assertNotContains(t, ir, "error.typed.nomatch")
}

func TestErrorHandlerBindingFieldAccess(t *testing.T) {
	ir := generateIR(t, `
		type IoError is error {
			int code;
		}
		fail!() void { raise IoError(message: "disk full", code: 28); }
		process!() int {
			fail() ? e is IoError { return e.code; };
			return 0;
		}
		main() { }
	`)
	// Should reconstruct value struct and access field
	assertContains(t, ir, "error.typed.match")
	assertContains(t, ir, "insertvalue { i8*, i8* }")
}

func TestErrorPositionalConstruction(t *testing.T) {
	ir := generateIR(t, `
		foo!() void { raise error("oops"); }
		main() { foo() ? e { }; }
	`)
	assertContains(t, ir, "error.handler")
}

func TestErrorSubtypePositionalConstruction(t *testing.T) {
	ir := generateIR(t, `
		type IoError is error { int code; }
		foo!() void { raise IoError("disk full", 28); }
		main() { foo() ? e { }; }
	`)
	assertContains(t, ir, "error.handler")
}

func TestGenericErrorTypeRaise(t *testing.T) {
	ir := generateIR(t, `
		type DataError[T] is error { T data; }
		foo!() void { raise DataError[int](message: "bad", data: 42); }
		main() { foo() ? e { }; }
	`)
	// Should monomorphize DataError[int]
	assertContains(t, ir, "DataError[int]")
	assertContains(t, ir, "error.handler")
}

func TestFailableCallInsideHandler(t *testing.T) {
	ir := generateIR(t, `
		parse!(string s) int { return 0; }
		foo!() int {
			int v = parse("x") ? e { return parse("0")?^; };
			return v;
		}
		main() { foo() ? e { }; }
	`)
	assertContains(t, ir, "error.handler")
	// The handler body should contain another error propagation
	assertContains(t, ir, "error.propagate")
}

func TestNestedErrorHandlers(t *testing.T) {
	ir := generateIR(t, `
		a!() int { return 1; }
		b!() int { return 2; }
		foo() {
			a() ? e1 {
				b() ? e2 { };
			};
		}
		main() { }
	`)
	// Should have multiple handler blocks
	assertContains(t, ir, "error.handler")
}

func TestErrorInheritanceChainTypedHandler(t *testing.T) {
	ir := generateIR(t, `
		type AppError is error { int code; }
		type DbError is AppError { string query; }
		fail!() void { raise DbError(message: "fail", code: 500, query: "SELECT"); }
		handler!() int {
			fail() ? e is AppError { return e.code; };
			return 0;
		}
		main() { }
	`)
	assertContains(t, ir, "error.typed.match")
	assertContains(t, ir, "promise_type_is")
}

func TestAutoPropagate(t *testing.T) {
	ir := generateIR(t, `
		fail!() void { raise error(message: "oops"); }
		process!() void {
			fail();
		}
		main() { }
	`)
	// Should have auto-propagation blocks
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
	// Should extract tag and conditionally branch
	assertContains(t, ir, "extractvalue { i1, i8* }")
	// Should return error result on error path
	assertContains(t, ir, "ret { i1, i8* }")
}

func TestAutoPropagate_NonVoid(t *testing.T) {
	ir := generateIR(t, `
		parse!() int { return 42; }
		process!() int {
			parse();
			return 0;
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

func TestAutoPropagateInTypedAssignment(t *testing.T) {
	ir := generateIR(t, `
		parse!() int { return 42; }
		wrapper!() int {
			int x = parse();
			return x;
		}
		main() { }
	`)
	// Should have auto-propagation blocks in wrapper
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
	// The ok path extracts the value (index 1 from failable result)
	assertContains(t, ir, "extractvalue { i1, i64, i8* }")
}

func TestAutoPropagateInInferredAssignment(t *testing.T) {
	ir := generateIR(t, `
		parse!() int { return 42; }
		wrapper!() int {
			x := parse();
			return x;
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

func TestAutoPropagateMultipleAssignments(t *testing.T) {
	ir := generateIR(t, `
		parse!(string s) int { return 0; }
		wrapper!() int {
			int a = parse("x");
			int b = parse("y");
			return a + b;
		}
		main() { }
	`)
	// Should have two sets of auto-propagation blocks
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

func TestAutoPropagateInFuncArg(t *testing.T) {
	ir := generateIR(t, `
		parse!() int { return 42; }
		use_value(int x) {}
		wrapper!() void {
			use_value(parse());
		}
		main() { }
	`)
	// Should have auto-propagation blocks for the argument
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
	// The ok path extracts the value (index 1 from failable result)
	assertContains(t, ir, "extractvalue { i1, i64, i8* }")
}

func TestAutoPropagateInMethodArg(t *testing.T) {
	ir := generateIR(t, `
		parse!() int { return 42; }
		type Foo { use_value(int x) {} }
		wrapper!() void {
			f := Foo();
			f.use_value(parse());
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

func TestAutoPropagateInConstructorArg(t *testing.T) {
	ir := generateIR(t, `
		parse!() int { return 42; }
		type Foo { int x; }
		wrapper!() void {
			Foo(x: parse());
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

func TestAutoPropagateMultipleArgs(t *testing.T) {
	ir := generateIR(t, `
		parse_a!() int { return 1; }
		parse_b!() int { return 2; }
		add(int a, int b) int { return a + b; }
		wrapper!() void {
			add(parse_a(), parse_b());
		}
		main() { }
	`)
	// Both arguments should have auto-propagation
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

func TestAutoPropagateInAssignStmt(t *testing.T) {
	ir := generateIR(t, `
		parse!() int { return 42; }
		wrapper!() int {
			int x = 0;
			x = parse();
			return x;
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

func TestAutoPropagateInExplicitNewArg(t *testing.T) {
	ir := generateIR(t, `
		parse!() int { return 42; }
		type Foo {
			int v;
			new(~this, int v) { this.v = v; }
		}
		wrapper!() void {
			Foo(v: parse());
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

func TestAutoPropagateInValueTypeArg(t *testing.T) {
	ir := generateIR(t, `
		parse!() int { return 42; }
		type Vec2 {
			int x `+"`"+`value;
			int y `+"`"+`value;
		}
		wrapper!() void {
			Vec2(x: parse(), y: 0);
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

func TestAutoPropagateInEnumVariantArg(t *testing.T) {
	ir := generateIR(t, `
		parse!() int { return 42; }
		enum Box { Val(int v) }
		wrapper!() void {
			Box.Val(v: parse());
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

func TestAutoPropagateInVecPushArg(t *testing.T) {
	ir := generateIR(t, `
		parse!() int { return 42; }
		wrapper!() void {
			int[] v = int[]();
			v.push(parse());
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

// B0323: Failable call result used as field access target must be unwrapped.
func TestAutoPropagateInFieldAccess(t *testing.T) {
	ir := generateIR(t, `
		type Foo { int x; }
		bar!() Foo { return Foo(x: 42); }
		wrapper!() int {
			return bar().x;
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

// B0323: Failable call result used as method call receiver must be unwrapped.
func TestAutoPropagateInMethodReceiver(t *testing.T) {
	ir := generateIR(t, `
		type Foo {
			int x;
			get_x(this) int { return this.x; }
		}
		bar!() Foo { return Foo(x: 42); }
		wrapper!() int {
			return bar().get_x();
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

// B0323: Failable call result used as generic method call receiver must be unwrapped.
func TestAutoPropagateInGenericMethodReceiver(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] {
			T val;
			cast[U](this, U default_val) U {
				return default_val;
			}
		}
		make_box!() Box[int] { return Box[int](val: 42); }
		wrapper!() string {
			return make_box().cast[string]("hello");
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

// B0322: Failable call used as string method receiver must auto-propagate.
func TestAutoPropagateMethodChainReceiver(t *testing.T) {
	ir := generateIR(t, `
		get_name!() string { return "hello"; }
		wrapper!() string {
			string s = get_name().trim();
			return s;
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

// B0322: Auto-propagate on vector .len access.
func TestAutoPropagateVectorLen(t *testing.T) {
	ir := generateIR(t, `
		make_vec!() int[] { return [1, 2]; }
		wrapper!() int {
			return make_vec().len;
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

// B0322: Auto-propagate on getter call.
func TestAutoPropagateGetterReceiver(t *testing.T) {
	ir := generateIR(t, `
		type Cnt {
			int n;
			get doubled int { return this.n * 2; }
		}
		make_cnt!() Cnt { return Cnt(n: 3); }
		wrapper!() int {
			return make_cnt().doubled;
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

// T0330: Failable call used as binary expression operand must auto-propagate.
func TestAutoPropagateInBinaryExpr(t *testing.T) {
	ir := generateIR(t, `
		read!() int { return 1; }
		wrapper!() bool {
			return read() != 0;
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
	assertContains(t, ir, "extractvalue { i1, i64, i8* }")
}

// T0330: Failable call used as unary expression operand must auto-propagate.
func TestAutoPropagateInUnaryExpr(t *testing.T) {
	ir := generateIR(t, `
		get_flag!() bool { return true; }
		wrapper!() bool {
			return !get_flag();
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

// T0330: Failable call as operand of && must auto-propagate in failable context.
func TestAutoPropagateInAndExpr(t *testing.T) {
	ir := generateIR(t, `
		flag!() bool { return true; }
		wrapper!() bool {
			return flag() && true;
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

// T0330: Failable call as operand of || must auto-propagate in failable context.
func TestAutoPropagateInOrExpr(t *testing.T) {
	ir := generateIR(t, `
		flag!() bool { return false; }
		wrapper!() bool {
			return false || flag();
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

// T0330: Failable call as elvis left operand must auto-propagate.
func TestAutoPropagateInElvisLeft(t *testing.T) {
	ir := generateIR(t, `
		get_opt!() int? { return 1; }
		wrapper!() int {
			return get_opt() ?: 0;
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

// T0330: Failable call as elvis right (default) operand must auto-propagate.
func TestAutoPropagateInElvisRight(t *testing.T) {
	ir := generateIR(t, `
		fallback!() int { return 0; }
		wrapper!(int? v) int {
			return v ?: fallback();
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

func TestRaiseExtractsInstancePtr(t *testing.T) {
	ir := generateIR(t, `
		type IoError is error { int code; }
		foo!() void { raise IoError(message: "err", code: 1); }
		main() { foo() ? e { }; }
	`)
	// Raise on user types should extract instance pointer (i8*) from value struct
	assertContains(t, ir, "extractvalue { i8*, i8* }")
}

func TestGenericFuncFailable(t *testing.T) {
	ir := generateIR(t, `
		tryIdentity![T](T x) T {
			return x;
		}
		main() {
			int v = tryIdentity[int](42)?!;
		}
	`)
	assertContains(t, ir, "define { i1, i64, i8* } @\"tryIdentity[int]\"")
}

func TestFailableDestructure(t *testing.T) {
	ir := generateIR(t, `
		parse!() int { return 42; }
		main() {
			(val, err) := parse();
		}
	`)
	// Should have branch on tag for error/ok paths
	assertContains(t, ir, "destruct.err")
	assertContains(t, ir, "destruct.ok")
	assertContains(t, ir, "destruct.merge")
	// Should alloca both bindings
	assertContains(t, ir, "%val")
	assertContains(t, ir, "%err")
}

func TestFailableDestructureDiscardError(t *testing.T) {
	ir := generateIR(t, `
		parse!() int { return 42; }
		main() {
			(val, _) := parse();
		}
	`)
	assertContains(t, ir, "destruct.merge")
	assertContains(t, ir, "%val")
}

// B0263: Failable destructure value must be freed at scope exit for heap user types.
func TestFailableDestructureValueFree(t *testing.T) {
	ir := generateIR(t, `
		type Pt { int x; int y; }
		make!() Pt { return Pt(x: 1, y: 2); }
		main() {
			(p, err) := make();
		}
	`)
	// The value variable 'p' should get a free binding (heap user type without drop).
	// emitFreeCall null-checks the instance pointer (safe for the error path's zeroinit).
	assertContains(t, ir, "free.call")
	assertContains(t, ir, "free.exec")
}

// T1160: the failable-unwrap layers (`?!`, `?^`) only extract the inner call's
// success value, so a discarded closure result must still be tracked through them.
func TestClosureFailableCallResultTrackedAsEnvTemp(t *testing.T) {
	ir := generateIR(t, `
		make_adder!(int x) () -> int { return || -> x + 1; }
		do_it() { make_adder(10)?!; }
		main() { do_it(); }
	`)
	body := extractFunction(ir, "__user.do_it")
	assertContains(t, body, "env.tmp.drop")
	assertContains(t, body, "env.tmp.exec")
}

// T1242: a failable getter discarded with `?!` (or `?^`) reaches
// closureResultMayAliasCallInput's MemberExpr arm. The outer genExpr(ErrorPanicExpr)
// calls trackUnwrappedFailableTemp with the extracted {fn,env} fat pointer; inside
// closureResultMayAliasCallInput the peel loop strips the ErrorPanicExpr, landing on
// the MemberExpr whose Target is the alias receiver.  T1227 ensures fresh getter
// results (no borrowed field returned), so the receiver has no closure field and the
// filter returns false — the env is registered and freed at statement end.
func TestClosureFailableGetterResultTracked(t *testing.T) {
	ir := generateIR(t, `
		type Maker { int base; get adder! () -> int { b := this.base; return move || -> b + 1; } }
		discard_failable() { m := Maker(base: 1); m.adder?!; }
		propagate_failable!() { m := Maker(base: 1); m.adder?^; }
		main() {}
	`)
	assertContains(t, extractFunction(ir, "__user.discard_failable"), "env.tmp.drop")
	assertContains(t, extractFunction(ir, "__user.propagate_failable"), "env.tmp.drop")
}

// T1160: the filter's ErrorPropagateExpr peel arm. `?^` yields the inner call's
// success value, so a discarded closure it produced is owned here and must be
// freed; the sibling `?!` arm is pinned by TestClosureFailableCallResultTrackedAsEnvTemp.
// Without the peel, `?^` lands in `default: return true` and the env leaks.
func TestClosurePropagatedFailableCallResultTrackedAsEnvTemp(t *testing.T) {
	ir := generateIR(t, `
		make_adder!(int x) () -> int { return || -> x + 1; }
		via_propagate!() { make_adder(10)?^; }
		main() { via_propagate()?!; }
	`)
	body := extractFunction(ir, "__user.via_propagate")
	assertContains(t, body, "env.tmp.drop")
	assertContains(t, body, "env.tmp.exec")
}

// A discard leaving its scope through an early `return` or a `raise` unwind must be
// cleaned on those exit edges, not only on block fallthrough.
func TestClosureCallResultDroppedOnReturnAndRaisePaths(t *testing.T) {
	ir := generateIR(t, `
		make_adder(int x) () -> int { return move || -> x + 1; }
		type Unwind is error { int code; }
		early(bool b) int {
			if b { make_adder(1); return 1; }
			return 0;
		}
		unwinding!(bool b) {
			make_adder(1);
			if b { raise Unwind(code: 1, message: "boom"); }
		}
		main() {}
	`)
	assertContains(t, extractFunction(ir, "__user.early"), "env.tmp.drop")
	assertContains(t, extractFunction(ir, "__user.unwinding"), "env.tmp.drop")
}

func TestStructuralSatisfactionWithoutMetaFails(t *testing.T) {
	// Without `structural meta, explicit `is is required
	src := `
		type Printable {
			print() string ` + "`" + `abstract;
		}
		type Doc {
			print() string { return "doc"; }
		}
		main() {
			Printable p = Doc();
		}
	`
	input := antlr.NewInputStream(src)
	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	pr := parser.NewPromiseParser(stream)
	pr.RemoveErrorListeners()
	tree := pr.CompilationUnit()
	file, errs := ast.Build("test.pr", tree)
	if len(errs) > 0 {
		t.Fatalf("AST build errors: %v", errs)
	}
	_, errs = sema.Check(file)
	if len(errs) == 0 {
		t.Error("expected sema error for assigning Doc to Printable without `structural, got none")
	}
}

func TestStructuralAdapterNonFailableToFailable(t *testing.T) {
	ir := generateIR(t, `
		type Processor `+"`"+`structural {
			process!(int x) int `+"`"+`abstract;
		}
		type Simple {
			process(int x) int { return x; }
		}
		main() {
			Processor p = Simple();
		}
	`)
	assertContains(t, ir, "Simple.process$view_adapt")
}

func TestPrimitiveToFailableStructuralView(t *testing.T) {
	// Primitive method is non-failable, interface method is failable
	// → adapter wraps result as success
	ir := generateIR(t, `
		type Converter `+"`"+`structural {
			to_string!() string `+"`"+`abstract;
		}
		convert(Converter c) string { return c.to_string()?!; }
		main() { convert(42); }
	`)
	assertContains(t, ir, "@promise_vtable_int_as_Converter")
	assertContains(t, ir, "int.to_string$view_adapt")
}

func TestReturnThisFailable(t *testing.T) {
	// Failable method returning `this` should also wrap into value struct.
	ir := generateIR(t, `
		type Widget {
			int id;
			clone!() Widget { return this; }
		}
		main() {
			w := Widget(id: 1);
			Widget w2 = w.clone()?!;
		}
	`)
	// Result type is { i1, { i8*, i8* }, i8* } (ok flag, value struct, error ptr)
	assertContains(t, ir, "define { i1, { i8*, i8* }, i8* } @Widget.clone(i8* %this)")
	assertContains(t, ir, "insertvalue { i8*, i8* }")
}

// T0275: Batch test main frees heap-allocated panic messages to prevent leak.
// Verifies: testPanicTypeGlobal declared, free_panic_msg block generated,
// and leak delta adjustment discounts the panic msg allocation.
func TestPanicMsgFreedInTestMain(t *testing.T) {
	src := `myTest() ` + "`test" + ` { }`
	result := compileResult(t, src)
	info, _ := sema.Check(func() *ast.File {
		input := antlr.NewInputStream(src)
		lexer := parser.NewPromiseLexer(input)
		lexer.RemoveErrorListeners()
		stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
		p := parser.NewPromiseParser(stream)
		p.RemoveErrorListeners()
		tree := p.CompilationUnit()
		file, _ := ast.Build("test.pr", tree)
		return file
	}())
	result.GenerateTestMain(info.Tests, nil)
	ir := result.Module.String()

	// T0275: __promise_test_panic_type global declared (non-TLS i8)
	assertContains(t, ir, "@__promise_test_panic_type = global i8 0")
	// T0275: free_panic_msg block conditionally frees heap panic msgs
	assertContains(t, ir, "free_panic_msg_myTest:")
	assertContains(t, ir, "after_free_panic_myTest:")
	// T0275: Leak delta adjustment — select discounts heap panic from delta
	assertContains(t, ir, "icmp eq i8")
}

// T0275: Test trampoline copies panic type to test harness global.
func TestTrampolineCopiesPanicType(t *testing.T) {
	src := `myTest() ` + "`test" + ` { }`
	result := compileResult(t, src)
	info, _ := sema.Check(func() *ast.File {
		input := antlr.NewInputStream(src)
		lexer := parser.NewPromiseLexer(input)
		lexer.RemoveErrorListeners()
		stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
		p := parser.NewPromiseParser(stream)
		p.RemoveErrorListeners()
		tree := p.CompilationUnit()
		file, _ := ast.Build("test.pr", tree)
		return file
	}())
	result.GenerateTestMain(info.Tests, nil)
	ir := result.Module.String()

	// The trampoline stores panic type to the non-TLS test global (load from TLS, store to non-TLS)
	assertContains(t, ir, "load i8, i8* @__promise_panic_type")
	assertContains(t, ir, "store i8 1, i8* @__promise_test_panic_type")
}

// --- Failable close() error propagation (B0013) ---

func TestUseFailableCloseErrorCapture(t *testing.T) {
	ir := generateIR(t, `
		type FRes {
			int id;
			close!(~this) { }
		}
		process!() {
			use r := FRes(id: 1);
			int x = r.id;
		}
	`)
	// Failable close should generate a result-type call (not void)
	assertContains(t, ir, "call { i1, i8* } @FRes.close")
	// Should have close error check and propagation blocks
	assertContains(t, ir, "close.err.flag")
	assertContains(t, ir, "close.err.ret")
}

func TestUseNonFailableCloseNoCapture(t *testing.T) {
	ir := generateIR(t, `
		type NRes {
			int id;
			close(~this) { }
		}
		process!() {
			use r := NRes(id: 1);
			int x = r.id;
		}
	`)
	// Non-failable close should remain a void call — no error capture
	assertContains(t, ir, "call void @NRes.close")
	assertNotContains(t, ir, "close.err.flag")
}

func TestUseFailableCloseSuppressedOnRaise(t *testing.T) {
	ir := generateIR(t, `
		type EBase is error { string message; }
		type FRes2 {
			int id;
			close!(~this) { }
		}
		process!() {
			use r := FRes2(id: 1);
			raise EBase(message: "fail");
		}
	`)
	// Raise path should suppress close errors (no close.err.flag on that path)
	// The close call is still emitted but result discarded
	assertContains(t, ir, "call { i1, i8* } @FRes2.close")
}

func TestUseFailableCloseInNonFailableFunc(t *testing.T) {
	ir := generateIR(t, `
		type FRes3 {
			int id;
			close!(~this) { }
		}
		process() {
			use r := FRes3(id: 1);
			int x = r.id;
		}
	`)
	// Non-failable function: close errors suppressed, no capture allocas
	assertNotContains(t, ir, "close.err.flag")
}

// T0135: Suppressed close errors are dropped (not leaked)
func TestSuppressedCloseErrorDropped(t *testing.T) {
	ir := generateIR(t, `
		type FRes {
			int id;
			close!(~this) { raise error(message: "close err"); }
		}
		process!() {
			use r := FRes(id: 1);
			raise error(message: "body err");
		}
	`)
	// Error-in-flight path: close error should be dropped via __mod_std_error.drop
	assertContains(t, ir, "close.err.drop")
	assertContains(t, ir, "@__mod_std_error.drop")
}

// T0135: Non-failable function drops suppressed close errors
func TestNonFailableSuppressedCloseErrorDropped(t *testing.T) {
	ir := generateIR(t, `
		type FRes {
			int id;
			close!(~this) { }
		}
		process() {
			use r := FRes(id: 1);
		}
	`)
	// Non-failable function: close error is suppressed and dropped
	assertContains(t, ir, "close.err.drop")
}

// T0135: Duplicate close error is dropped when first error already captured
func TestDuplicateCloseErrorDropped(t *testing.T) {
	ir := generateIR(t, `
		type FRes {
			int id;
			close!(~this) { }
		}
		process!() {
			use a := FRes(id: 1);
			use b := FRes(id: 2);
		}
	`)
	// Multiple failable closes: second error should be dropped
	assertContains(t, ir, "close.err.drop.dup")
}

// T0135: Failable result capture registers error optional for drop
func TestFailableResultCaptureErrorDrop(t *testing.T) {
	ir := generateIR(t, `
		fail!() { raise error(message: "test"); }
		test() {
			(val, err) := fail();
		}
	`)
	// Error optional should have a drop flag and be dropped at scope exit
	assertContains(t, ir, "err.dropflag")
	assertContains(t, ir, "optdrop.check")
}

func TestUseFailableCloseVirtualDispatchCapture(t *testing.T) {
	ir := generateIR(t, `
		type Conn {
			int fd;
			close!() { }
		}
		type TcpConn is Conn {
			close!() { }
		}
		process!() {
			use c := Conn(fd: 3);
			int x = c.fd;
		}
	`)
	// Virtual dispatch + failable function: close error should be captured
	assertContains(t, ir, "@promise_vtable_Conn")
	assertContains(t, ir, "close.err.flag")
	assertContains(t, ir, "close.err.ret")
}

// Drop with early return in failable function
func TestDropWithEarlyReturnFailable(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		work!() void {
			r := Resource(id: 42);
			return;
		}
		main() { }
	`)
	// drop() should be emitted before the return
	assertContains(t, ir, "call void @Resource.drop")
}

// Drop with raise: cleanup before error return
func TestDropWithRaise(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		fail!() void {
			r := Resource(id: 1);
			raise error(message: "oops");
		}
		main() { }
	`)
	// drop() should be emitted before the raise
	assertContains(t, ir, "call void @Resource.drop")
}

// Error propagation triggers scope cleanup
func TestDropErrorPropagateCleansUp(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		risky!() int {
			return 42;
		}
		work!() int {
			r := Resource(id: 1);
			int val = risky()?^;
			return val + r.id;
		}
		main() { }
	`)
	assertContains(t, ir, "call void @Resource.drop")
	assertContains(t, ir, "define { i1, i64, i8* } @__user.work")
}

// T0086: Error types now get bindingFree at scope exit (previously excluded)
func TestBindingFreeErrorType(t *testing.T) {
	ir := generateIR(t, `
		main() {
			error e = error("test");
			string msg = e.message;
		}
	`)
	assertContains(t, ir, "e.dropflag")
	assertContains(t, ir, "call void @pal_free")
}

// T0086: Raising a local error variable clears its drop flag before scope cleanup
func TestRaiseLocalErrorClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		fail!() void {
			error e = error("boom");
			raise e;
		}
		main() { }
	`)
	// The error should get a drop flag
	assertContains(t, ir, "e.dropflag")
	// The drop flag should be cleared (store false) before scope cleanup
	assertContains(t, ir, "store i1 false")
}

// T0103: String temp cleanup on error propagation path
func TestStringTempCleanupOnErrorPath(t *testing.T) {
	ir := generateIR(t, `
		fail!() int {
			raise error(message: "fail");
		}
		use_both(string s, int x) int {
			return x;
		}
		work!() int {
			return use_both("hello".to_upper(), fail());
		}
		main() {}
	`)
	// Should have error-path temp cleanup blocks (T0103)
	assertContains(t, ir, "err.tmp.drop")
	assertContains(t, ir, "err.tmp.skip")
	assertContains(t, ir, "call void @promise_string_drop")
}

// T0091: Error types get synthesized drop (frees message string field + instance)
func TestSynthDropIncludesErrorTypes(t *testing.T) {
	ir := generateIR(t, `
		main() {
			error e = error(message: "fail");
		}
	`)
	// error gets a synthesized drop that frees its string message field
	assertContains(t, ir, "define void @__mod_std_error.drop")
}

// T0083/T0091: Caught error instances are dropped after handler blocks
func TestErrorHandlerDropsInstance(t *testing.T) {
	ir := generateIR(t, `
		fail!() int {
			raise error(message: "boom");
		}
		main() {
			int v = fail()? e => 0;
		}
	`)
	// Handler block should drop the error instance via synthesized drop
	assertContains(t, ir, "call void @__mod_std_error.drop")
}

// T0083/T0091/T0110: Typed error handler uses child type's drop for match path
func TestTypedErrorHandlerDropsInstance(t *testing.T) {
	ir := generateIR(t, `
		type IoError is error { int code; }
		fail!() int {
			raise IoError(code: 1, message: "io");
		}
		main() {
			int v = fail()? e is IoError { 0; } else { -1; };
		}
	`)
	// Match path drops via IoError.drop (resolves child type, T0110)
	assertContains(t, ir, "call void @IoError.drop")
	// Else path drops via base error.drop (unknown concrete type)
	assertContains(t, ir, "call void @__mod_std_error.drop")
}

// T0110: Error type synthesized drop includes string field drops
func TestErrorTypeSynthDropIncludesStringFields(t *testing.T) {
	ir := generateIR(t, `
		main() {
			error e = error(message: "fail");
		}
	`)
	// error.drop should call promise_string_drop for the message field
	assertContains(t, ir, "define void @__mod_std_error.drop")
	assertContains(t, ir, "call void @promise_string_drop")
}

// T0110: Child error type drop frees child-specific string fields
func TestChildErrorTypeSynthDropFreesChildFields(t *testing.T) {
	ir := generateIR(t, `
		type NotFoundError is error { string key; }
		fail!() int {
			raise NotFoundError(key: "missing", message: "not found");
		}
		main() {
			int v = fail()? e is NotFoundError { 0; } else { -1; };
		}
	`)
	// NotFoundError.drop should be defined (synthesized)
	assertContains(t, ir, "define void @NotFoundError.drop")
	// Match path uses NotFoundError.drop, not error.drop
	assertContains(t, ir, "call void @NotFoundError.drop")
	// NotFoundError.drop should have 2 string drops (message + key)
	dropBody := extractFunction(ir, "NotFoundError.drop")
	count := strings.Count(dropBody, "call void @promise_string_drop")
	if count < 2 {
		t.Errorf("expected at least 2 promise_string_drop calls in NotFoundError.drop (message + key), got %d\nBody:\n%s", count, dropBody)
	}
}

// T0110: Dup-on-field-access works for error types (prevents use-after-free)
func TestErrorFieldAccessDupsString(t *testing.T) {
	ir := generateIR(t, `
		type NotFoundError is error { string key; }
		fail!() string {
			raise NotFoundError(key: "missing", message: "not found");
		}
		main() {
			string s = fail()? e is NotFoundError { e.key; } else { ""; };
		}
	`)
	// Accessing e.key in error handler should dup the string via string_new (copy)
	// dupString() calls promise_string_new to create a heap copy of the field data
	assertContains(t, ir, "strdup.copy")
}

// B0225: goroutine_exit frees G.panic_msg when panicked==2 (heap-allocated msg).
func TestGoroutineExitFreePanicMsg(t *testing.T) {
	ir := generateIR(t, `
		main() { }
	`)
	// goroutine_exit should have free_panic_msg and do_free_g blocks
	assertContains(t, ir, "free_panic_msg:")
	assertContains(t, ir, "do_free_g:")
}

// B0228: promise_panic_msg stores type=2 in TLS panic_type to mark heap-allocated msg.
func TestPanicMsgSetsHeapPanickedFlag(t *testing.T) {
	ir := generateIR(t, `
		panic_msg(string msg) `+"`"+`extern("promise_panic_msg");
		main() { panic_msg("boom"); }
	`)
	// promise_panic_msg stores i8 2 in @__promise_panic_type (heap-allocated)
	assertContains(t, ir, "store i8 2, i8* @__promise_panic_type")
}

// B0228: promise_panic is no longer noreturn — call sites use ret instead of unreachable.
func TestPanicNotNoreturn(t *testing.T) {
	ir := generateIR(t, `
		main() {}
	`)
	// promise_panic should NOT have noreturn (other funcs like pal_exit still do)
	assertNotContains(t, ir, "declare void @promise_panic(i8*) noreturn")
	// The function body should end with ret void in the set_panic block
	assertContains(t, ir, "define void @promise_panic(i8*")
}

// B0228: promise_panic double-panic check aborts with exit code 134.
func TestPanicDoublePanicAbort(t *testing.T) {
	ir := generateIR(t, `
		main() {}
	`)
	// Double panic should load flag, compare, and branch to abort path
	assertContains(t, ir, "load i8, i8* @__promise_panic_flag")
	assertContains(t, ir, "call void @pal_exit(i32 134)")
}

// B0228: Category A — OOB panic returns instead of unreachable.
func TestOOBPanicReturns(t *testing.T) {
	ir := generateIR(t, `
		get(int[] v, int i) int { return v[i]; }
		main() { v := [1]; get(v, 0); }
	`)
	// The OOB panic block should call promise_panic then return (not unreachable)
	assertContains(t, ir, "call void @promise_panic(")
	// promise_panic declaration should NOT have noreturn (other funcs like pal_exit still do)
	assertNotContains(t, ir, "declare void @promise_panic(i8*) noreturn")
}

// T0147: Panic check emitted after every call expression.
func TestPanicCheckAfterCallExpr(t *testing.T) {
	ir := generateIR(t, `
		foo() {}
		main() { foo(); }
	`)
	// After the call to foo(), emitPanicCheck should emit:
	// - load of __promise_panic_flag
	// - icmp ne (check if flag is set)
	// - conditional branch to panic.cleanup / panic.ok
	assertContains(t, ir, "panic.cleanup")
	assertContains(t, ir, "panic.ok")
}

// T0147: Panic check after method call.
func TestPanicCheckAfterMethodCall(t *testing.T) {
	ir := generateIR(t, `
		type Foo {
			int x;
			bar(this) int { return this.x; }
		}
		main() { f := Foo(x: 1); f.bar(); }
	`)
	assertContains(t, ir, "panic.cleanup")
	assertContains(t, ir, "panic.ok")
}

// T0147: Go-call (direct) coroutine has panic exit block.
func TestPanicCheckGoCallDirect(t *testing.T) {
	ir := generateIR(t, `
		work() {}
		main() { go work(); }
	`)
	// genGoCallExpr should emit go.panic_exit block
	assertContains(t, ir, "go.panic_exit")
	assertContains(t, ir, "go.call_ok")
}

// T0148: genGoCallExprViaBlock has final panic check before final suspend.
func TestPanicCheckGoCallViaBlockFinal(t *testing.T) {
	ir := generateIR(t, `
		type Foo {
			bar(this) {}
		}
		main() {
			f := Foo();
			go f.bar();
		}
	`)
	// genGoCallExprViaBlock should emit go.panic_exit block
	assertContains(t, ir, "go.panic_exit")
	// The coroutine body should have a final panic flag check (icmp ne + cond br)
	assertContains(t, ir, "@__promise_panic_flag")
}

func TestVariadicFailableIR(t *testing.T) {
	// Variadic + failable function in IR.
	ir := generateIR(t, `
		trySum!(...int nums) int {
			if nums.len == 0 { raise error(message: "empty"); }
			int total = 0;
			for n in nums { total += n; }
			return total;
		}
		main() {
			x := trySum(1, 2, 3)?!;
		}
	`)
	// Failable returns {i1, i64, i8*} (error flag + result + error ptr)
	assertContains(t, ir, "define { i1, i64, i8* } @__user.trySum(i8* %nums)")
}

func TestVariadicVectorHeapTempOnFailableArg(t *testing.T) {
	// B0201: When a failable arg inside a variadic call fails, the vector
	// allocated for variadic args must be freed on the error path.
	ir := generateIR(t, `
		sum(...int nums) int {
			int total = 0;
			for n in nums { total += n; }
			return total;
		}
		parse!(string s) int {
			raise error(message: s);
		}
		foo!() int {
			return sum(parse("a"), parse("b"));
		}
		main() { foo()?!; }
	`)
	// The variadic vector should be tracked as a heap temp (pal_alloc + store to alloca)
	// and freed on the error propagation path (err.heap.drop block calls pal_free)
	assertContains(t, ir, "err.heap.drop")
	assertContains(t, ir, "call void @pal_free")
}

// TestFailableSetterInstanceNonVirtualIR verifies that a `set name!(...)`
// setter call site auto-propagates the failable result in a failable enclosing
// function. T0708.
func TestFailableSetterInstanceNonVirtualIR(t *testing.T) {
	ir := generateIR(t, `
		type Foo {
			int x;
			get count int { return this.x; }
			set count!(int v) {
				if v < 0 { raise error("negative"); }
				this.x = v;
			}
		}
		main!() {
			f := Foo(x: 0);
			f.count = -5;
		}
	`)
	// The setter is defined with a failable result type.
	assertContains(t, ir, "define { i1, i8* } @Foo.count$set")
	// Call site captures the result and routes through auto-propagation.
	assertContains(t, ir, "call { i1, i8* } @Foo.count$set")
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
	assertContains(t, ir, "extractvalue { i1, i8* }")
}

// TestFailableGlobalSetterIR — `global failable setter (T0703 + T0708).
func TestFailableGlobalSetterIR(t *testing.T) {
	ir := generateIR(t, "type Foo {\n"+
		"int x;\n"+
		"get count int `global { return 0; }\n"+
		"set count!(int v) `global { if v < 0 { raise error(\"neg\"); } }\n"+
		"}\n"+
		"main!() {\n"+
		"Foo.count = -5;\n"+
		"}\n")
	assertContains(t, ir, "define { i1, i8* } @Foo.count$set(i64 %v)")
	assertContains(t, ir, "call { i1, i8* } @Foo.count$set(i64")
	assertContains(t, ir, "auto.propagate")
}

// TestFailableIndexSetterIR — a failable []= method's call site auto-propagates.
func TestFailableIndexSetterIR(t *testing.T) {
	ir := generateIR(t, `
		type Box {
			int x;
			[](int k) int { return this.x; }
			[]=!(int k, int v) { if v < 0 { raise error("neg"); } this.x = v; }
		}
		main!() {
			b := Box(x: 0);
			b[0] = -5;
		}
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "extractvalue { i1, i8* }")
}

// TestFailableSliceSetterIR — a failable [:]= method's call site auto-propagates.
func TestFailableSliceSetterIR(t *testing.T) {
	ir := generateIR(t, `
		type Box {
			int x;
			[:](int? low, int? high) int { return this.x; }
			[:]=!(int? low, int? high, int v) { if v < 0 { raise error("neg"); } this.x = v; }
		}
		main!() {
			b := Box(x: 0);
			b[1:2] = -5;
		}
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "extractvalue { i1, i8* }")
}

// TestFailableSetterCompoundAssignIR — `f.count += v` reads via getter then
// writes via setter; if either is failable both must propagate. This test
// covers the setter side.
func TestFailableSetterCompoundAssignIR(t *testing.T) {
	ir := generateIR(t, `
		type Foo {
			int x;
			get count int { return this.x; }
			set count!(int v) {
				if v < 0 { raise error("neg"); }
				this.x = v;
			}
		}
		main!() {
			f := Foo(x: 0);
			f.count += 5;
		}
	`)
	assertContains(t, ir, "call { i1, i8* } @Foo.count$set")
	assertContains(t, ir, "auto.propagate")
}

// TestFailableGetterCompoundAssignIR — `f.count += v` reads the current value
// via a failable getter; the read must auto-propagate and the arithmetic must
// operate on the extracted ok value, not the failable result struct. T0709.
func TestFailableGetterCompoundAssignIR(t *testing.T) {
	ir := generateIR(t, `
		type Foo {
			int x;
			get count! int {
				if this.x < 0 { raise error("neg"); }
				return this.x;
			}
			set count(int v) { this.x = v; }
		}
		main!() {
			f := Foo(x: 0);
			f.count += 5;
		}
	`)
	// Getter is defined with a failable result type.
	assertContains(t, ir, "define { i1, i64, i8* } @Foo.count")
	// Read routes through auto-propagation and the op uses the ok payload (field 1).
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
	assertContains(t, ir, "extractvalue { i1, i64, i8* }")
	// The arithmetic must NOT be applied to the raw failable struct (the old bug).
	assertNotContains(t, ir, "add { i1, i64, i8* }")
}

// TestFailableIndexGetterCompoundIR — `b[0] += v` reads via a failable [] getter
// (non-optional return), which must auto-propagate before the op. T0709.
func TestFailableIndexGetterCompoundIR(t *testing.T) {
	ir := generateIR(t, `
		type Box {
			int x;
			[]!(int k) int {
				if this.x < 0 { raise error("neg"); }
				return this.x;
			}
			[]=(int k, int v) { this.x = v; }
		}
		main!() {
			b := Box(x: 0);
			b[0] += 5;
		}
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "extractvalue { i1, i64, i8* }")
	assertNotContains(t, ir, "add { i1, i64, i8* }")
}

// TestFailableIndexGetterIncDecIR — `b[0]++` reads via a failable [] getter,
// which must auto-propagate before the increment. T0709.
func TestFailableIndexGetterIncDecIR(t *testing.T) {
	ir := generateIR(t, `
		type Box {
			int x;
			[]!(int k) int {
				if this.x < 0 { raise error("neg"); }
				return this.x;
			}
			[]=(int k, int v) { this.x = v; }
		}
		main!() {
			b := Box(x: 0);
			b[0]++;
		}
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "extractvalue { i1, i64, i8* }")
	assertNotContains(t, ir, "add { i1, i64, i8* }")
}

// TestFailableGetterCompoundInGenericMethodIR — a failable getter compound inside
// a monomorphized generic method body exercises unwrapFailableCompoundRead's
// typeSubst path (c.typeSubst != nil). The operand is a concrete int field, so
// the native += is valid while the method itself is mono'd per instantiation. T0709.
func TestFailableGetterCompoundInGenericMethodIR(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] {
			T payload;
			int counter;
			get count! int {
				if this.counter < 0 { raise error("neg"); }
				return this.counter;
			}
			set count(int v) { this.counter = v; }
			bump!(~this) { this.count += 1; }
		}
		main!() {
			b := Box[int](payload: 0, counter: 10);
			b.bump();
		}
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "extractvalue { i1, i64, i8* }")
	assertNotContains(t, ir, "add { i1, i64, i8* }")
}

// TestFailablePropertyIncDecIR: T0712. With a failable getter and setter, the
// getter result is unwrapped (extractvalue from {i1, i64, i8*}) before the op,
// and the setter result auto-propagates — no malformed `add { i1` on the struct.
func TestFailablePropertyIncDecIR(t *testing.T) {
	ir := generateIR(t, `
		type Foo {
			int x;
			get count! int { if this.x < 0 { raise error("neg"); } return this.x; }
			set count!(int v) { if v < 0 { raise error("neg"); } this.x = v; }
		}
		main!() {
			f := Foo(x: 0);
			f.count++;
		}
	`)
	assertContains(t, ir, "define { i1, i64, i8* } @Foo.count(")
	assertContains(t, ir, "extractvalue { i1, i64, i8* }")
	assertContains(t, ir, "call { i1, i8* } @Foo.count$set")
	assertContains(t, ir, "auto.propagate")
	assertNotContains(t, ir, "add { i1")
}

func TestFailableGetterResultType(t *testing.T) {
	ir := generateIR(t, `
		type MyErr is error { int code; }
		type Foo {
			int _val;
			get value! int {
				if this._val < 0 { raise MyErr(code: 1, message: "neg"); }
				return this._val;
			}
		}
		main() {
			Foo f = Foo(_val: 42);
			int v = f.value?!;
		}
	`)
	// Failable getter should return result type {i1, i64, i8*}
	assertContains(t, ir, "define { i1, i64, i8* } @Foo.value(")
}

func TestFailableGetterVirtualDispatch(t *testing.T) {
	ir := generateIR(t, `
		type MyErr is error { int code; }
		type Base {
			get value! int `+"`"+`abstract;
		}
		type Impl is Base {
			int _v;
			get value! int { return this._v; }
		}
		main() {
			Base b = Impl(_v: 10);
			int v = b.value?!;
		}
	`)
	// Abstract failable getter should use vtable dispatch
	assertContains(t, ir, "@promise_vtable_Base")
	assertContains(t, ir, "@promise_vtable_Impl")
	assertContains(t, ir, "define { i1, i64, i8* } @Impl.value(")
}

func TestFailableGetterStringResult(t *testing.T) {
	ir := generateIR(t, `
		type MyErr is error { int code; }
		type Foo {
			int _mode;
			get label! string {
				if this._mode < 0 { raise MyErr(code: 1, message: "bad"); }
				return "ok";
			}
		}
		main() {
			Foo f = Foo(_mode: 1);
			string s = f.label?!;
		}
	`)
	// Failable getter returning string should have result type in signature
	assertContains(t, ir, "define { i1, i8*, i8* } @Foo.label(")
}

func TestEnumMethodFailable(t *testing.T) {
	ir := generateIR(t, `
		enum Mode { A, B,
			check!(this) string {
				match this {
					Mode.A => { return "a"; },
					Mode.B => { return "b"; },
				}
			}
		}
		main() { string s = Mode.A.check()?!; }
	`)
	// Failable method returns result struct
	assertContains(t, ir, "@Mode.check(i8* %this)")
}

// T1172: a fixed-array-returning function whose body contains a panic-capable
// operation (string concat) reaches the panic-cleanup return (emitPanicReturn),
// which must emit the array zero aggregate — NOT the i64-0 default that produced
// malformed `ret i64 0` in a `[N x T]`-returning function.
func TestT1172ArrayReturnPanicCleanupZeroValue(t *testing.T) {
	ir := generateIR(t, `
		enum ArrHolder { Pair(string[2] a), Empty }
		mk() string[2] {
			ArrHolder h = ArrHolder.Pair(a: ["x" + "1", "y" + "2"]);
			return ["z", "w"];
		}
		main() { a := mk(); }
	`)
	fn := extractFunction(ir, "__user.mk")
	// The panic-cleanup path returns a zeroinitializer of the array type, never
	// a bare i64 0.
	assertContains(t, fn, "ret [2 x i8*] zeroinitializer")
	assertNotContains(t, fn, "ret i64 0")
}

func TestIsGenericErrorHandler(t *testing.T) {
	ir := generateIR(t, `
		type AppError[T] is error { T detail; }
		do_thing!() AppError[int] {
			raise AppError[int](message: "err", detail: 42);
		}
		main() {
			do_thing() ? e is AppError[int] {
			}!;
		}
	`)
	assertContains(t, ir, "call i32 @promise_type_is")
	assertContains(t, ir, "promise_typeinfo_AppError")
}

// B0134: generic error type constructor inside generic function body
// must be collected for monomorphization via func instance substitution.
func TestGenericErrorTypeInGenericFuncBody(t *testing.T) {
	ir := generateIR(t, `
		type AppError[T] is error { T detail; }
		make_err![T](T detail) AppError[T] {
			raise AppError[T](message: "fail", detail: detail);
		}
		main() { make_err[int](42) ? e { }; }
	`)
	// B0134: AppError[int] must be monomorphized from the generic function body
	assertContains(t, ir, "AppError[int]")
}

func TestT1073RaiseForceUnwrapNeutralizesSource(t *testing.T) {
	ir := generateIR(t, `
		type T1073Err is error { string d; }
		rz!(T1073Err? move o) int { raise o!; }
		main() {}
	`)
	fn := extractFunction(ir, "__user.rz")
	if fn == "" {
		t.Fatal("expected __user.rz in IR")
	}
	assertContains(t, fn, t1073NeutralizeSig)
}

// B0262: Discarded auto-propagated failable call returning heap user type should drop+free.
func TestDropDiscardedAutoPropagateUserType(t *testing.T) {
	ir := generateIR(t, `
		type Foo {
			string name;
			drop(~this) {}
		}
		make_foo!() Foo { return Foo(name: "x"); }
		test!() {
			make_foo();
		}
	`)
	testFn := extractFunction(ir, "__user.test")
	assertContains(t, testFn, "autoprop.drop")
	assertContains(t, testFn, "call void @Foo.drop")
}

// B0262: Discarded auto-propagated failable call returning closure should free env.
func TestDropDiscardedAutoPropagateClosureEnv(t *testing.T) {
	ir := generateIR(t, `
		make_fn!() (int) -> int {
			int x = 42;
			(int) -> int f = |int y| -> x + y;
			return f;
		}
		test!() {
			make_fn();
		}
	`)
	testFn := extractFunction(ir, "__user.test")
	assertContains(t, testFn, "autoprop.env.free")
}

// B0226: Untyped error handler should use RTTI-based drop dispatch.
func TestUntypedErrorRttiDrop(t *testing.T) {
	ir := generateIR(t, `
		type MyError is error { int code; }
		fail_my!() void { raise MyError(message: "err", code: 42); }
		main() {
			fail_my()? e {
			};
		}
	`)
	// B0226: Should emit RTTI-based drop dispatch (loads drop fn from typeinfo)
	assertContains(t, ir, "rtti.drop")
}

// B0325: Field access on a ?! unwrap result must track the intermediate heap instance.
func TestFieldAccessOnErrorPanicResultTracked(t *testing.T) {
	ir := generateIR(t, `
		type Pair { int x; int y; }
		make_pair!() Pair { return Pair(x: 10, y: 20); }
		test!() {
			int v = make_pair()?!.x;
		}
	`)
	assertContains(t, ir, "heap.drop")
	assertContains(t, ir, "call void @pal_free(")
}

// B0325: Method call on a ?! unwrap result must track the intermediate heap instance.
func TestMethodCallOnErrorPanicResultTracked(t *testing.T) {
	ir := generateIR(t, `
		type Pair { int x; int y;
			sum(this) int { return this.x + this.y; }
		}
		make_pair!() Pair { return Pair(x: 10, y: 20); }
		test!() {
			int v = make_pair()?!.sum();
		}
	`)
	assertContains(t, ir, "heap.drop")
	assertContains(t, ir, "call void @pal_free(")
}

// TestT0545_GenericIndirectionBackstopNoPanic was retired: T0616 now rejects
// generic-indirection clone of a single-owner handle at the call site (sema),
// so the codegen backstop path the test exercised is unreachable for
// well-formed user code. The compile-fail behavior is verified by
// TestT0616_VectorCloneInGenericTaskError in the sema package.

// TestT0482_NestedHandleBackstopNoPanic pins the codegen backstop for the
// nested-Named-handle shape behind generic indirection: a generic function
// clones a Vector[T] and is instantiated with a user type that owns a Task
// through a field. Direct user code is now gated by the T0482 sema predicate
// (firstNestedSingleOwnerHandle), but a generic body is checked with unbound
// T, so the concrete Holder reaches dupHeapValueFields at codegen. The
// isOpaqueContainerType skip (compiler.go:dupHeapValueFields) must degrade the
// nested Task field to a shallow copy without a Go panic (the recursive
// dupHeapValue would otherwise bitcast an i8* handle to a struct type and
// crash). The residual runtime double-free behind generic indirection is the
// separate, tracked T0616 — this test only generates IR, never runs it.
func TestT0482_NestedHandleBackstopNoPanic(t *testing.T) {
	ir := generateIR(t, `
		worker_int() int { return 42; }
		type Holder { Task[int] t; }
		dup_holders[T](Vector[T] v) Vector[T] { return v.clone(); }
		test_nh() {
			v := Vector[Holder]();
			v.push(Holder(t: go worker_int()));
			v2 := dup_holders[Holder](v);
		}
	`)
	// The Holder-instantiated generic dup function must be emitted (proves the
	// nested-handle backstop path was exercised in codegen, not skipped).
	assertContains(t, ir, "dup_holders[Holder]")
}

// T1634: an indirect (closure) call to a *failable* function type must type the
// function pointer with the callee's result struct — `{ i1, T, i8* }` — not the
// bare `T` from sig.Result(). The thunk copies the real function's RetType, so
// building the pointer from sig.Result() alone made the two disagree: the call
// was typed to return `i64` while the callee returned `{ i1, i64, i8* }`, and
// unwrapping it (`?^`) panicked codegen with "types.Type is *types.IntType,
// not *types.StructType".
func TestT1634_FailableFunctionTypeIndirectCallResultShape(t *testing.T) {
	ir := generateIR(t, `
		boom!(int x) int { return x * 2; }
		test_indirect!() {
			!(int) -> int f = boom;
			int r = f(3)?^;
		}
	`)
	// The function pointer the indirect call is bitcast to must carry the
	// failable result struct, with the env pointer as the first parameter.
	assertContains(t, ir, "{ i1, i64, i8* } (i8*, i64)*")
}

// T1634: `!() -> void` — a failable function with nothing to return uses the
// two-field result struct `{ i1, i8* }`.
func TestT1634_FailableVoidFunctionTypeIndirectCallResultShape(t *testing.T) {
	ir := generateIR(t, `
		vboom!() { }
		test_indirect_void!() {
			!() -> void v = vboom;
			v()?^;
		}
	`)
	assertContains(t, ir, "{ i1, i8* } (i8*)*")
}

// T1634: a non-failable function type is unaffected — its indirect call still
// returns the bare result type.
func TestT1634_NonFailableFunctionTypeIndirectCallResultShape(t *testing.T) {
	ir := generateIR(t, `
		test_plain() {
			(int) -> int f = |int x| -> x * 2;
			int r = f(3);
		}
	`)
	assertContains(t, ir, "i64 (i8*, i64)*")
	assertNotContains(t, ir, "{ i1, i64, i8* } (i8*, i64)*")
}
