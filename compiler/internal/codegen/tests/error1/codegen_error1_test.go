package error1

import (
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

func TestPanicBody(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {}
	`)
	// B0228: promise_panic now sets TLS flag and returns (no longjmp/exit)
	codegentest.AssertContains(t, ir, "define void @promise_panic(i8*")
	codegentest.AssertContains(t, ir, "store i8 1, i8* @__promise_panic_flag")     // set panic flag
	codegentest.AssertContains(t, ir, "store i8* %msg, i8** @__promise_panic_msg") // store msg
	codegentest.AssertContains(t, ir, "store i8 1, i8* @__promise_panic_type")     // type=1 (.rodata)
	codegentest.AssertContains(t, ir, "fatal: panic during panic recovery")        // double-panic message
	codegentest.AssertContains(t, ir, "call void @pal_exit(i32 134)")              // double-panic exit
}

func TestPanicMsgBody(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		panic_msg(string msg) `+"`"+`extern("promise_panic_msg");
		main() { panic_msg("boom"); }
	`)
	codegentest.AssertContains(t, ir, "define void @promise_panic_msg(i8*")
	codegentest.AssertContains(t, ir, "bitcast i8* %msg to %promise_string_v*")
	// B0228: promise_panic_msg now sets TLS flag and returns (no longjmp/exit)
	codegentest.AssertContains(t, ir, "store i8 1, i8* @__promise_panic_flag") // set panic flag
	codegentest.AssertContains(t, ir, "store i8* %")                           // store C string msg
	codegentest.AssertContains(t, ir, "store i8 2, i8* @__promise_panic_type") // type=2 (heap)
	codegentest.AssertContains(t, ir, "call i8* @pal_alloc(")                  // allocate C string copy
	codegentest.AssertContains(t, ir, "fatal: panic during panic recovery")    // double-panic message
}

func TestFailableNewConstructorCodegen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "define { i1, i8* } @Port.new(i8* %this")
	// Constructor call should call new and check the error
	codegentest.AssertContains(t, ir, "call { i1, i8* } @Port.new(")
}

func TestGenericFailableFactoryPassthrough(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "call { i1, { i8*, i8* }, i8* } @Strict.tryParse(")
	// Failable passthrough: tryLoad[Strict] should return the result directly
	// (single ret of the call result, no insertvalue wrapping)
	codegentest.AssertContains(t, ir, "@\"tryLoad[Strict]\"(")
	codegentest.AssertContains(t, ir, "ret { i1, { i8*, i8* }, i8* } %")
}

func TestFailableDeclaration(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		parse!(string s) int { return 0; }
		main() { }
	`)
	// Return type should be result struct { i1, i64, i8* }
	codegentest.AssertContains(t, ir, "define { i1, i64, i8* } @__user.parse(i8* %s)")
}

func TestReturnInFailable(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		parse!(string s) int { return 42; }
		main() { }
	`)
	// Should wrap value in Ok result: tag=false, value, null error
	codegentest.AssertContains(t, ir, "insertvalue { i1, i64, i8* }")
	codegentest.AssertContains(t, ir, "i1 false")
	codegentest.AssertContains(t, ir, "ret { i1, i64, i8* }")
}

func TestFailableVoidBangShorthand(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		fail!() { raise error(message: "oops"); }
		main() { }
	`)
	// Should produce void result struct { i1, i8* }
	codegentest.AssertContains(t, ir, "define { i1, i8* } @__user.fail()")
}

func TestFailableMain(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main!() {
			raise error(message: "boom");
		}
	`)
	// Body compiled into helper function
	codegentest.AssertContains(t, ir, "define { i1, i8* } @__promise_main_body()")
	// Error path panics
	codegentest.AssertContains(t, ir, "unhandled error in main")
}

func TestRaiseStmt(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		parse!(string s) int { raise error(message: "parse error"); }
		main() { }
	`)
	// Should wrap error in Error result: tag=true
	codegentest.AssertContains(t, ir, "i1 true")
	codegentest.AssertContains(t, ir, "ret { i1, i64, i8* }")
	// Should create the error message string
	codegentest.AssertContains(t, ir, `c"parse error"`)
	// Should extract instance pointer from value struct
	codegentest.AssertContains(t, ir, "extractvalue { i8*, i8* }")
}

func TestErrorPropagate(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		parse!(string s) int { return 0; }
		process!() int {
			x := parse("42")?^;
			return x;
		}
		main() { }
	`)
	// Should have propagation and ok blocks
	codegentest.AssertContains(t, ir, "error.propagate")
	codegentest.AssertContains(t, ir, "error.ok")
	// Should extract tag from result
	codegentest.AssertContains(t, ir, "extractvalue { i1, i64, i8* }")
}

func TestErrorUnwrap(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		parse!(string s) int { return 0; }
		main() {
			x := parse("42")?!;
		}
	`)
	// Should have panic and ok blocks
	codegentest.AssertContains(t, ir, "error.panic")
	codegentest.AssertContains(t, ir, "error.ok")
	// B0200: Should extract message string from error instance before panicking.
	// The error.panic block must bitcast the error instance to load the message
	// field, then create a C string copy for promise_panic.
	codegentest.AssertContains(t, ir, "call void @promise_panic(")
	codegentest.AssertContains(t, ir, "unreachable")
	// Verify message extraction: bitcast to error instance type, GEP to message field
	codegentest.AssertContains(t, ir, "getelementptr %promise_error_i")
}

// B0256: emitErrorPanic heap-allocates a C string copy but promise_panic sets
// type=1 (.rodata). The fix overwrites panic_type to 2 (heap) after the call
// so goroutine_exit frees it. T0142: now calls promise_panic_at which handles
// the type=2 store internally.
func TestErrorPanicSetsHeapType(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		parse!(string s) int { return 0; }
		main() {
			x := parse("42")?!;
		}
	`)
	// emitErrorPanic calls promise_panic_at (T0142)
	codegentest.AssertContains(t, ir, "call void @promise_panic_at(")
	// promise_panic_at body stores type=2 after calling promise_panic
	codegentest.AssertContains(t, ir, "store i8 2, i8* @__promise_panic_type")
}

// T0142: error panic via ?! includes source file and line number.
func TestErrorPanicSourceLocation(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		fail!() int { return 0; }
		main() {
			x := fail()?!;
		}
	`)
	// Should call promise_panic_at with a filename global and line number constant
	codegentest.AssertContains(t, ir, "call void @promise_panic_at(")
	// Filename global should be a .file. prefixed constant
	codegentest.AssertContains(t, ir, "@.file.")
	// promise_panic_at should be defined with its body (not just declared)
	codegentest.AssertContains(t, ir, "define void @promise_panic_at(")
}

// T0125: When func()?! returns a string, the unwrapped i8* must be tracked
// as a stmt temp so it gets freed at statement end if not claimed.
func TestErrorUnwrapStringTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		make_str!() string { return "hello"; }
		main() {
			int n = make_str()?!.len;
		}
	`)
	// Should have string temp tracking: store to alloca + drop flag
	codegentest.AssertContains(t, ir, "tmp.drop")
	codegentest.AssertContains(t, ir, "promise_string_drop")
}

// B0260: When func()?^ propagates a string result, the ok-path i8* must be
// tracked as a stmt temp so it gets freed if not claimed (e.g., by vec.push
// which dups the string).
func TestErrorPropagateStringTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		make_str!() string { return "hello"; }
		wrap!(string[] move v) string[] {
			v.push(make_str()?^);
			return v;
		}
	`)
	// Should have string temp tracking for the propagated string result
	codegentest.AssertContains(t, ir, "error.propagate")
	codegentest.AssertContains(t, ir, "error.ok")
	// The decoded string must be tracked as a temp and dropped after push
	codegentest.AssertContains(t, ir, "tmp.drop")
	codegentest.AssertContains(t, ir, "promise_string_drop")
}

func TestErrorHandler(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		parse!(string s) int { return 0; }
		main() {
			x := parse("42") ? e { 0; };
		}
	`)
	// Should have handler, ok, and merge blocks
	codegentest.AssertContains(t, ir, "error.handler")
	codegentest.AssertContains(t, ir, "error.ok")
	codegentest.AssertContains(t, ir, "error.merge")
}

func TestErrorHandlerDiscard(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		parse!(string s) int { return 0; }
		main() {
			x := parse("42") ? _ { 0; };
		}
	`)
	codegentest.AssertContains(t, ir, "error.handler")
	codegentest.AssertContains(t, ir, "error.ok")
}

func TestVoidFailable(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		validate!(string s) void { return; }
		main() { }
	`)
	// Return type should be { i1, i8* }
	codegentest.AssertContains(t, ir, "define { i1, i8* } @__user.validate(i8* %s)")
}

func TestVoidRaise(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		validate!(string s) void { raise error(message: "invalid"); }
		main() { }
	`)
	codegentest.AssertContains(t, ir, "i1 true")
	codegentest.AssertContains(t, ir, "ret { i1, i8* }")
}

func TestFailableMethod(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Parser {
			string input;
			parse!(this) int `+"`structural(protocol: false)"+` {
				return 42;
			}
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "define { i1, i64, i8* } @Parser.parse(i8* %this)")
}

func TestFailableAutoTerminator(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		validate!(string s) void {
			if true {
				return;
			}
		}
		main() { }
	`)
	// Auto-terminator on fall-through path should wrap in Ok (tag=false)
	codegentest.AssertContains(t, ir, "i1 false")
	codegentest.AssertContains(t, ir, "ret { i1, i8* }")
}

func TestVoidFailablePropagate(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		validate!(string s) void { raise error(message: "invalid"); }
		process!() void {
			validate("x")?^;
		}
		main() { }
	`)
	// Should propagate error from void failable callee
	codegentest.AssertContains(t, ir, "error.propagate")
	codegentest.AssertContains(t, ir, "error.ok")
	// Callee returns { i1, i8* }, caller also returns { i1, i8* }
	codegentest.AssertContains(t, ir, "extractvalue { i1, i8* }")
}

func TestVoidFailableUnwrap(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		validate!(string s) void { raise error(message: "invalid"); }
		main() {
			validate("x")?!;
		}
	`)
	codegentest.AssertContains(t, ir, "error.panic")
	codegentest.AssertContains(t, ir, "error.ok")
	codegentest.AssertContains(t, ir, "call void @promise_panic(")
}

func TestVoidFailableHandler(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		validate!(string s) void { raise error(message: "invalid"); }
		main() {
			validate("x") ? e { };
		}
	`)
	codegentest.AssertContains(t, ir, "error.handler")
	codegentest.AssertContains(t, ir, "error.ok")
	codegentest.AssertContains(t, ir, "error.merge")
}

func TestNestedErrorPropagation(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		a!() int { return 1; }
		b!() int { return a()?^; }
		c!() int { return b()?^; }
		main() { }
	`)
	// Both b and c should have propagation blocks
	codegentest.AssertContains(t, ir, "error.propagate")
	codegentest.AssertContains(t, ir, "error.ok")
}

func TestErrorHandlerWithReturn(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		parse!(string s) int { return 0; }
		process(string s) int {
			x := parse(s) ? e { return -1; };
			return x;
		}
		main() { }
	`)
	// Handler block should contain a return (terminator)
	codegentest.AssertContains(t, ir, "error.handler")
	codegentest.AssertContains(t, ir, "error.ok")
}

func TestFailableConditionalRaiseReturn(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		parse!(string s) int {
			if s == "" {
				raise error(message: "empty");
			}
			return 42;
		}
		main() { }
	`)
	// Should have both Ok and Error paths
	codegentest.AssertContains(t, ir, "i1 true")
	codegentest.AssertContains(t, ir, "i1 false")
	codegentest.AssertContains(t, ir, "ret { i1, i64, i8* }")
}

// --- Typed Error Handler Tests ---

func TestTypedErrorHandler(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "call i32 @promise_type_is(")
	// Should have typed match/nomatch blocks
	codegentest.AssertContains(t, ir, "error.typed.match")
	codegentest.AssertContains(t, ir, "error.typed.nomatch")
}

func TestTypedErrorHandlerInFailable(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "error.typed.nomatch")
	codegentest.AssertContains(t, ir, "ret { i1, i8* }")
}

func TestTypedErrorHandlerNomatchPropagates(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "error.typed.nomatch")
	codegentest.AssertContains(t, ir, "ret { i1, i8* }")
}

func TestTypedErrorHandlerDiscardBinding(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type IoError is error {
			int code;
		}
		fail!() void { raise IoError(message: "disk full", code: 28); }
		process!() void {
			fail() ? _ is IoError { };
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "call i32 @promise_type_is(")
	codegentest.AssertContains(t, ir, "error.typed.match")
}

func TestTypedErrorHandlerElse(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type IoError is error { int code; }
		fail!() void { raise IoError(message: "disk full", code: 28); }
		process() {
			fail() ? e is IoError { } else { };
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "error.typed.match")
	codegentest.AssertContains(t, ir, "error.typed.nomatch")
	// No panic in nomatch — else handles it
	codegentest.AssertNotContains(t, ir, "unhandled error type")
}

func TestTypedErrorHandlerElseWithBinding(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type IoError is error { int code; }
		fail!() void { raise IoError(message: "disk full", code: 28); }
		get_msg() string {
			fail() ? e is IoError { return "io"; } else e { return e.message; };
			return "";
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "error.typed.match")
	codegentest.AssertContains(t, ir, "error.typed.nomatch")
}

func TestTypedErrorHandlerBang(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type IoError is error { int code; }
		fail!() void { raise IoError(message: "disk full", code: 28); }
		process() {
			fail() ? e is IoError { }!;
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "error.typed.match")
	codegentest.AssertContains(t, ir, "error.typed.nomatch")
	// Nomatch panics via promise_panic
	codegentest.AssertContains(t, ir, "call void @promise_panic(")
	codegentest.AssertContains(t, ir, "unreachable")
}

func TestUntypedErrorHandlerUnchanged(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		fail!() void { raise error(message: "oops"); }
		main() {
			fail() ? e { };
		}
	`)
	// Untyped handler should NOT have typed match/nomatch blocks
	codegentest.AssertContains(t, ir, "error.handler")
	codegentest.AssertNotContains(t, ir, "error.typed.match")
	codegentest.AssertNotContains(t, ir, "error.typed.nomatch")
}

func TestErrorHandlerBindingFieldAccess(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "error.typed.match")
	codegentest.AssertContains(t, ir, "insertvalue { i8*, i8* }")
}

func TestErrorPositionalConstruction(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		foo!() void { raise error("oops"); }
		main() { foo() ? e { }; }
	`)
	codegentest.AssertContains(t, ir, "error.handler")
}

func TestErrorSubtypePositionalConstruction(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type IoError is error { int code; }
		foo!() void { raise IoError("disk full", 28); }
		main() { foo() ? e { }; }
	`)
	codegentest.AssertContains(t, ir, "error.handler")
}

func TestGenericErrorTypeRaise(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type DataError[T] is error { T data; }
		foo!() void { raise DataError[int](message: "bad", data: 42); }
		main() { foo() ? e { }; }
	`)
	// Should monomorphize DataError[int]
	codegentest.AssertContains(t, ir, "DataError[int]")
	codegentest.AssertContains(t, ir, "error.handler")
}

func TestFailableCallInsideHandler(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		parse!(string s) int { return 0; }
		foo!() int {
			int v = parse("x") ? e { return parse("0")?^; };
			return v;
		}
		main() { foo() ? e { }; }
	`)
	codegentest.AssertContains(t, ir, "error.handler")
	// The handler body should contain another error propagation
	codegentest.AssertContains(t, ir, "error.propagate")
}

func TestNestedErrorHandlers(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "error.handler")
}

func TestErrorInheritanceChainTypedHandler(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type AppError is error { int code; }
		type DbError is AppError { string query; }
		fail!() void { raise DbError(message: "fail", code: 500, query: "SELECT"); }
		handler!() int {
			fail() ? e is AppError { return e.code; };
			return 0;
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "error.typed.match")
	codegentest.AssertContains(t, ir, "promise_type_is")
}

func TestAutoPropagate(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		fail!() void { raise error(message: "oops"); }
		process!() void {
			fail();
		}
		main() { }
	`)
	// Should have auto-propagation blocks
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "auto.ok")
	// Should extract tag and conditionally branch
	codegentest.AssertContains(t, ir, "extractvalue { i1, i8* }")
	// Should return error result on error path
	codegentest.AssertContains(t, ir, "ret { i1, i8* }")
}

func TestAutoPropagate_NonVoid(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		parse!() int { return 42; }
		process!() int {
			parse();
			return 0;
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "auto.ok")
}

func TestAutoPropagateInTypedAssignment(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		parse!() int { return 42; }
		wrapper!() int {
			int x = parse();
			return x;
		}
		main() { }
	`)
	// Should have auto-propagation blocks in wrapper
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "auto.ok")
	// The ok path extracts the value (index 1 from failable result)
	codegentest.AssertContains(t, ir, "extractvalue { i1, i64, i8* }")
}

func TestAutoPropagateInInferredAssignment(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		parse!() int { return 42; }
		wrapper!() int {
			x := parse();
			return x;
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "auto.ok")
}

func TestAutoPropagateMultipleAssignments(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		parse!(string s) int { return 0; }
		wrapper!() int {
			int a = parse("x");
			int b = parse("y");
			return a + b;
		}
		main() { }
	`)
	// Should have two sets of auto-propagation blocks
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "auto.ok")
}

func TestAutoPropagateInFuncArg(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		parse!() int { return 42; }
		use_value(int x) {}
		wrapper!() void {
			use_value(parse());
		}
		main() { }
	`)
	// Should have auto-propagation blocks for the argument
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "auto.ok")
	// The ok path extracts the value (index 1 from failable result)
	codegentest.AssertContains(t, ir, "extractvalue { i1, i64, i8* }")
}

func TestAutoPropagateInMethodArg(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		parse!() int { return 42; }
		type Foo { use_value(int x) {} }
		wrapper!() void {
			f := Foo();
			f.use_value(parse());
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "auto.ok")
}

func TestAutoPropagateInConstructorArg(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		parse!() int { return 42; }
		type Foo { int x; }
		wrapper!() void {
			Foo(x: parse());
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "auto.ok")
}

func TestAutoPropagateMultipleArgs(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		parse_a!() int { return 1; }
		parse_b!() int { return 2; }
		add(int a, int b) int { return a + b; }
		wrapper!() void {
			add(parse_a(), parse_b());
		}
		main() { }
	`)
	// Both arguments should have auto-propagation
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "auto.ok")
}

func TestAutoPropagateInAssignStmt(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		parse!() int { return 42; }
		wrapper!() int {
			int x = 0;
			x = parse();
			return x;
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "auto.ok")
}

func TestAutoPropagateInExplicitNewArg(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "auto.ok")
}

func TestAutoPropagateInValueTypeArg(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "auto.ok")
}

func TestAutoPropagateInEnumVariantArg(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		parse!() int { return 42; }
		enum Box { Val(int v) }
		wrapper!() void {
			Box.Val(v: parse());
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "auto.ok")
}

func TestAutoPropagateInVecPushArg(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		parse!() int { return 42; }
		wrapper!() void {
			int[] v = int[]();
			v.push(parse());
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "auto.ok")
}

// B0323: Failable call result used as field access target must be unwrapped.
func TestAutoPropagateInFieldAccess(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Foo { int x; }
		bar!() Foo { return Foo(x: 42); }
		wrapper!() int {
			return bar().x;
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "auto.ok")
}

// B0323: Failable call result used as method call receiver must be unwrapped.
func TestAutoPropagateInMethodReceiver(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "auto.ok")
}

// B0323: Failable call result used as generic method call receiver must be unwrapped.
func TestAutoPropagateInGenericMethodReceiver(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "auto.ok")
}

// B0322: Failable call used as string method receiver must auto-propagate.
func TestAutoPropagateMethodChainReceiver(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		get_name!() string { return "hello"; }
		wrapper!() string {
			string s = get_name().trim();
			return s;
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "auto.ok")
}

// B0322: Auto-propagate on vector .len access.
func TestAutoPropagateVectorLen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		make_vec!() int[] { return [1, 2]; }
		wrapper!() int {
			return make_vec().len;
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "auto.ok")
}

// B0322: Auto-propagate on getter call.
func TestAutoPropagateGetterReceiver(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "auto.ok")
}

// T0330: Failable call used as binary expression operand must auto-propagate.
func TestAutoPropagateInBinaryExpr(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		read!() int { return 1; }
		wrapper!() bool {
			return read() != 0;
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "auto.ok")
	codegentest.AssertContains(t, ir, "extractvalue { i1, i64, i8* }")
}

// T0330: Failable call used as unary expression operand must auto-propagate.
func TestAutoPropagateInUnaryExpr(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		get_flag!() bool { return true; }
		wrapper!() bool {
			return !get_flag();
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "auto.ok")
}

// T0330: Failable call as operand of && must auto-propagate in failable context.
func TestAutoPropagateInAndExpr(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		flag!() bool { return true; }
		wrapper!() bool {
			return flag() && true;
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "auto.ok")
}

// T0330: Failable call as operand of || must auto-propagate in failable context.
func TestAutoPropagateInOrExpr(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		flag!() bool { return false; }
		wrapper!() bool {
			return false || flag();
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "auto.ok")
}

// T0330: Failable call as elvis left operand must auto-propagate.
func TestAutoPropagateInElvisLeft(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		get_opt!() int? { return 1; }
		wrapper!() int {
			return get_opt() ?: 0;
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "auto.ok")
}

// T0330: Failable call as elvis right (default) operand must auto-propagate.
func TestAutoPropagateInElvisRight(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		fallback!() int { return 0; }
		wrapper!(int? v) int {
			return v ?: fallback();
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "auto.ok")
}

func TestRaiseExtractsInstancePtr(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type IoError is error { int code; }
		foo!() void { raise IoError(message: "err", code: 1); }
		main() { foo() ? e { }; }
	`)
	// Raise on user types should extract instance pointer (i8*) from value struct
	codegentest.AssertContains(t, ir, "extractvalue { i8*, i8* }")
}

func TestGenericFuncFailable(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		tryIdentity![T](T x) T {
			return x;
		}
		main() {
			int v = tryIdentity[int](42)?!;
		}
	`)
	codegentest.AssertContains(t, ir, "define { i1, i64, i8* } @\"tryIdentity[int]\"")
}

func TestFailableDestructure(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		parse!() int { return 42; }
		main() {
			(val, err) := parse();
		}
	`)
	// Should have branch on tag for error/ok paths
	codegentest.AssertContains(t, ir, "destruct.err")
	codegentest.AssertContains(t, ir, "destruct.ok")
	codegentest.AssertContains(t, ir, "destruct.merge")
	// Should alloca both bindings
	codegentest.AssertContains(t, ir, "%val")
	codegentest.AssertContains(t, ir, "%err")
}
