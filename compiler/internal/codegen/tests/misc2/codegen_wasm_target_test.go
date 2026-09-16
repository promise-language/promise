package misc2

import (
	"bytes"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen"
	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// --- Extern print (struct-based ABI) ---

func TestPrintStringExtern(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		print_s(string s) `+"`"+`extern("promise_print_string");
		main() { print_s("hello"); }
	`)
	codegentest.AssertContains(t, ir, "%promise_string_v = type")
	codegentest.AssertContains(t, ir, "define void @promise_print_string(i8*")
}

// --- Extern architecture tests ---

func TestExternCustomCName(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		log_value(int x) `+"`"+`extern("my_log_int");
		main() { log_value(99); }
	`)
	codegentest.AssertContains(t, ir, "declare void @my_log_int(i8*")
	codegentest.AssertContains(t, ir, "call void @my_log_int(i8*")
}

func TestExternDefaultCName(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		do_thing(int x) `+"`"+`extern;
		main() { do_thing(1); }
	`)
	codegentest.AssertContains(t, ir, "declare void @promise_do_thing(i8*")
}

// --- wall-clock (realtime) extern body tests (T0962) ---

// The time module's promise_wallclock extern gets its body from defineTimeBodies.
// On POSIX it reads CLOCK_REALTIME (id 0 on both Linux and macOS), distinct from
// the monotonic nanotime read (id 1 on Linux / 6 on macOS).
func TestWallclockExternBodyPosix(t *testing.T) {
	ir := codegentest.GenerateIRForTarget(t, `
		_wallclock() int `+"`extern(\"promise_wallclock\")"+`;
		main() { int _x = _wallclock(); }
	`, "x86_64-unknown-linux-gnu")
	codegentest.AssertContains(t, ir, "define void @promise_wallclock(i8* %sret)")
	// CLOCK_REALTIME is 0 — the realtime clock, not the monotonic one.
	codegentest.AssertContains(t, ir, "call i32 @clock_gettime(i32 0,")
}

// On wasm32-wasi the body reads CLOCK_REALTIME (clockid 0) via the WASI
// clock_time_get import — the same import the monotonic clock uses (clockid 1),
// distinct only in the clockid argument (T1067).
func TestWallclockExternBodyWasiRealtime(t *testing.T) {
	ir := codegentest.GenerateIRForTarget(t, `
		_wallclock() int `+"`extern(\"promise_wallclock\")"+`;
		main() { int _x = _wallclock(); }
	`, "wasm32-wasi")
	codegentest.AssertContains(t, ir, "define void @promise_wallclock(i8* %sret)")
	// Realtime clockid is 0, passed to the WASI clock_time_get import.
	codegentest.AssertContains(t, ir, "call i32 @clock_time_get(i32 0,")
	if strings.Contains(ir, "@clock_gettime") {
		t.Errorf("wasm32-wasi wallclock body must not call clock_gettime\ngot:\n%s", ir)
	}
}

// On wasm32-web there is no guaranteed realtime source, so the body returns a
// constant 0 — no WASI clock_time_get import and no POSIX clock_gettime (T1067).
func TestWallclockExternBodyWasmWebReturnsZero(t *testing.T) {
	ir := codegentest.GenerateIRForTarget(t, `
		_wallclock() int `+"`extern(\"promise_wallclock\")"+`;
		main() { int _x = _wallclock(); }
	`, "wasm32-web")
	codegentest.AssertContains(t, ir, "define void @promise_wallclock(i8* %sret)")
	if strings.Contains(ir, "@clock_time_get") {
		t.Errorf("wasm32-web wallclock body must not import clock_time_get\ngot:\n%s", ir)
	}
	if strings.Contains(ir, "@clock_gettime") {
		t.Errorf("wasm32-web wallclock body must not call clock_gettime\ngot:\n%s", ir)
	}
}

// On Windows the body reads GetSystemTimePreciseAsFileTime and converts the
// FILETIME (100ns ticks since 1601) to nanoseconds since the Unix epoch.
func TestWallclockExternBodyWindows(t *testing.T) {
	ir := codegentest.GenerateIRForTarget(t, `
		_wallclock() int `+"`extern(\"promise_wallclock\")"+`;
		main() { int _x = _wallclock(); }
	`, "x86_64-pc-windows-msvc")
	codegentest.AssertContains(t, ir, "define void @promise_wallclock(i8* %sret)")
	codegentest.AssertContains(t, ir, "@GetSystemTimePreciseAsFileTime")
	// Unix-epoch shift constant: 116444736000000000 ticks from 1601 → 1970.
	codegentest.AssertContains(t, ir, "116444736000000000")
}

func TestWasmExternDirectReturn(t *testing.T) {
	ir := codegentest.GenerateIRForTarget(t, `
		_sched_yield() i32 `+"`extern(\"__wasi_sched_yield\") `wasm_import(\"wasi_snapshot_preview1\", \"sched_yield\") `target(wasm)"+`;
		main() {}
	`, "wasm32-wasi")
	codegentest.AssertContains(t, ir, "declare i32 @__wasi_sched_yield()")
}

func TestWasmExternDirectParams(t *testing.T) {
	ir := codegentest.GenerateIRForTarget(t, `
		_fd_close(i32 fd) `+"`extern(\"fd_close\") `wasm_import(\"wasi_snapshot_preview1\", \"fd_close\") `target(wasm)"+`;
		main() {}
	`, "wasm32-wasi")
	codegentest.AssertContains(t, ir, "declare void @fd_close(i32 %fd)")
}

func TestWasmExternDirectReturnWithParams(t *testing.T) {
	ir := codegentest.GenerateIRForTarget(t, `
		_fd_read(i32 fd, i32 iovs, i32 iovs_len, i32 nwritten) i32 `+"`extern(\"fd_read\") `wasm_import(\"wasi_snapshot_preview1\", \"fd_read\") `target(wasm)"+`;
		main() {}
	`, "wasm32-wasi")
	codegentest.AssertContains(t, ir, "declare i32 @fd_read(i32 %fd, i32 %iovs, i32 %iovs_len, i32 %nwritten)")
}

func TestWasmExternDirectCall(t *testing.T) {
	ir := codegentest.GenerateIRForTarget(t, `
		_get() int `+"`extern(\"test_get\") `wasm_import(\"env\", \"test_get\") `target(wasm)"+`;
		main() { x := _get(); }
	`, "wasm32-wasi")
	codegentest.AssertContains(t, ir, "declare i64 @test_get()")
	codegentest.AssertContains(t, ir, "call i64 @test_get()")
}

func TestWasmExternDirectCallWithParams(t *testing.T) {
	ir := codegentest.GenerateIRForTarget(t, `
		_add(int a, int b) int `+"`extern(\"test_add\") `wasm_import(\"env\", \"test_add\") `target(wasm)"+`;
		main() { x := _add(1, 2); }
	`, "wasm32-wasi")
	codegentest.AssertContains(t, ir, "declare i64 @test_add(i64 %a, i64 %b)")
	codegentest.AssertContains(t, ir, "call i64 @test_add(i64")
}

func TestWasmExternNativeUnchanged(t *testing.T) {
	// Native targets still use sret/i8* for the same types
	ir := codegentest.GenerateIR(t, `
		get_value() int `+"`"+`extern("native_get");
		use_value(int x) `+"`"+`extern("native_use");
		main() { use_value(1); x := get_value(); }
	`)
	codegentest.AssertContains(t, ir, "declare void @native_get(i8* %sret)")
	codegentest.AssertContains(t, ir, "declare void @native_use(i8* %x)")
}

func TestWasmExternBoolReturn(t *testing.T) {
	ir := codegentest.GenerateIRForTarget(t, `
		_check() bool `+"`extern(\"test_check\") `wasm_import(\"env\", \"test_check\") `target(wasm)"+`;
		main() { x := _check(); }
	`, "wasm32-wasi")
	// Bool (i1) should use direct return on WASM, not sret
	codegentest.AssertContains(t, ir, "declare i1 @test_check()")
	codegentest.AssertContains(t, ir, "call i1 @test_check()")
}

func TestWasmExternF64Param(t *testing.T) {
	ir := codegentest.GenerateIRForTarget(t, `
		_set(f64 val) `+"`extern(\"test_set\") `wasm_import(\"env\", \"test_set\") `target(wasm)"+`;
		main() { _set(3.14); }
	`, "wasm32-wasi")
	// f64 (double) should use direct param on WASM
	codegentest.AssertContains(t, ir, "declare void @test_set(double %val)")
}

// T1506: wasm_import string PARAMETERS must flatten to a canonical (ptr, len)
// pair, matching what host (JS) glue expects — not a pointer to Promise's
// private boxed-string value struct. Before this fix, a wasm_import function
// taking a plain `string` param received a single i8* pointing to a
// {vtable=null, instance} value-struct alloca, so real host code receiving the
// call saw one opaque pointer argument instead of the expected (ptr, len).
// The canonical (ptr, len) shape is the one documented in docs/wasm-bindings.md
// §"Canonical ABI Representation"; no host should need to know the boxed layout.
func TestWasmImportStringParamFlattensToPtrLen(t *testing.T) {
	ir := codegentest.GenerateIRForTarget(t, `
		take_string(string s) `+"`extern(\"take_string\") `wasm_import(\"dbg\", \"take_string\") `target(web)"+`;
		main() { take_string("hello"); }
	`, "wasm32-web")
	codegentest.AssertContains(t, ir, `declare void @take_string(i8* %s_ptr, i32 %s_len) `)
	codegentest.AssertContains(t, ir, `"wasm-import-name"="take_string"`)
	codegentest.AssertContains(t, ir, "call void @take_string(i8*")
}

// T1506: the flattening must be scoped to wasm_import externs only — a plain
// (non-wasm_import) native extern taking a string param keeps passing the boxed
// value-struct pointer, which is the convention the codegen-synthesized platform
// layer expects (promise_print_string and the promise_io_*/promise_os_* bridges
// all receive their string that way).
//
// T1660: this comment used to cite cabi_string_data as the caller relying on
// that shape, which was exactly backwards — wasm_alloc.c's helpers are plain C
// and want the bare instance pointer. They are now lowered through the raw-C-ABI
// registry instead; see TestCabiHelpersUseRawCABI.
func TestWasmExternStringParamUnchangedForNonWasmImport(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		_take(string s) `+"`extern(\"native_take\")"+`;
		main() { _take("hello"); }
	`)
	codegentest.AssertContains(t, ir, "declare void @native_take(i8* %s)")
}

func TestWasmExternFailableStillSret(t *testing.T) {
	ir := codegentest.GenerateIRForTarget(t, `
		_open!(i32 fd) i32 `+"`extern(\"test_open\") `wasm_import(\"env\", \"test_open\") `target(wasm)"+`;
		main() {}
	`, "wasm32-wasi")
	// Failable externs always use sret, even on WASM
	codegentest.AssertContains(t, ir, "declare void @test_open(i8* %sret")
}

// T0315: wasm32-web targets must export @_initialize (the JS/Node entry point)
// rather than @_start (the WASI Command convention).
func TestWasmWebEmitsInitialize(t *testing.T) {
	ir := codegentest.GenerateIRForTarget(t, `
		main() { print_line("hi"); }
	`, "wasm32-web")
	codegentest.AssertContains(t, ir, "define void @_initialize()")
	codegentest.AssertNotContains(t, ir, "define void @_start()")
}

// wasm32-wasi keeps the existing @_start export.
func TestWasmWasiEmitsStart(t *testing.T) {
	ir := codegentest.GenerateIRForTarget(t, `
		main() { print_line("hi"); }
	`, "wasm32-wasi")
	codegentest.AssertContains(t, ir, "define void @_start()")
	codegentest.AssertNotContains(t, ir, "define void @_initialize()")
}

func TestExternMultipleParams(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		add_ext(int a, int b) `+"`"+`extern("test_add");
		main() { add_ext(1, 2); }
	`)
	codegentest.AssertContains(t, ir, "declare void @test_add(i8* %a, i8* %b)")
	codegentest.AssertContains(t, ir, "call void @test_add")
}

func TestExternReturnValue(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		get_value() int `+"`"+`extern("test_get");
		main() { x := get_value(); }
	`)
	// sret: struct return becomes void with first param as result pointer
	codegentest.AssertContains(t, ir, "declare void @test_get(i8* %sret)")
	// Return value should be loaded from sret alloca and unpacked
	codegentest.AssertContains(t, ir, "extractvalue %promise_int_v")
}

func TestExternStructTypeDefs(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		use_int(int x) `+"`"+`extern("test_use_int");
		main() { use_int(42); }
	`)
	// All four struct types should be defined
	codegentest.AssertContains(t, ir, "%promise_int_t = type {}")
	codegentest.AssertContains(t, ir, "%promise_int_m = type { %promise_int_t* }")
	codegentest.AssertContains(t, ir, "%promise_int_i = type { %promise_int_m* }")
	codegentest.AssertContains(t, ir, "%promise_int_v = type { i8*, %promise_int_i*, i64 }")
}

// --- Primitive type layout coverage ---
// These tests verify that layout computation and extern declarations work
// for all primitive types. Externs are declared but not called since sema
// doesn't allow implicit narrowing from int/f64 literals to narrow types.

func TestExternI8Layout(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		log_i8(i8 x) `+"`"+`extern("test_i8");
		main() { }
	`)
	codegentest.AssertContains(t, ir, "%promise_i8_v = type { i8*, %promise_i8_i*, i8 }")
	codegentest.AssertContains(t, ir, "%promise_i8_i = type { %promise_i8_m* }")
	codegentest.AssertContains(t, ir, "%promise_i8_m = type { %promise_i8_t* }")
	codegentest.AssertContains(t, ir, "%promise_i8_t = type {}")
	codegentest.AssertContains(t, ir, "declare void @test_i8(i8*")
}

func TestExternI16Layout(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		log_i16(i16 x) `+"`"+`extern("test_i16");
		main() { }
	`)
	codegentest.AssertContains(t, ir, "%promise_i16_v = type { i8*, %promise_i16_i*, i16 }")
	codegentest.AssertContains(t, ir, "declare void @test_i16(i8*")
}

func TestExternI32Layout(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		log_i32(i32 x) `+"`"+`extern("test_i32");
		main() { }
	`)
	codegentest.AssertContains(t, ir, "%promise_i32_v = type { i8*, %promise_i32_i*, i32 }")
	codegentest.AssertContains(t, ir, "declare void @test_i32(i8*")
}

func TestExternU8Layout(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		log_u8(u8 x) `+"`"+`extern("test_u8");
		main() { }
	`)
	codegentest.AssertContains(t, ir, "%promise_u8_v = type { i8*, %promise_u8_i*, i8 }")
	codegentest.AssertContains(t, ir, "declare void @test_u8(i8*")
}

func TestExternU16Layout(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		log_u16(u16 x) `+"`"+`extern("test_u16");
		main() { }
	`)
	codegentest.AssertContains(t, ir, "%promise_u16_v = type { i8*, %promise_u16_i*, i16 }")
	codegentest.AssertContains(t, ir, "declare void @test_u16(i8*")
}

func TestExternU32Layout(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		log_u32(u32 x) `+"`"+`extern("test_u32");
		main() { }
	`)
	codegentest.AssertContains(t, ir, "%promise_u32_v = type { i8*, %promise_u32_i*, i32 }")
	codegentest.AssertContains(t, ir, "declare void @test_u32(i8*")
}

func TestExternU64Layout(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		log_u64(u64 x) `+"`"+`extern("test_u64");
		main() { }
	`)
	codegentest.AssertContains(t, ir, "%promise_u64_v = type { i8*, %promise_u64_i*, i64 }")
	codegentest.AssertContains(t, ir, "declare void @test_u64(i8*")
}

func TestExternI64Layout(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		log_i64(i64 x) `+"`"+`extern("test_i64");
		main() { }
	`)
	codegentest.AssertContains(t, ir, "%promise_i64_v = type { i8*, %promise_i64_i*, i64 }")
	codegentest.AssertContains(t, ir, "declare void @test_i64(i8*")
}

func TestExternF32Layout(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		log_f32(f32 x) `+"`"+`extern("test_f32");
		main() { }
	`)
	codegentest.AssertContains(t, ir, "%promise_f32_v = type { i8*, %promise_f32_i*, float }")
	codegentest.AssertContains(t, ir, "declare void @test_f32(i8*")
}

func TestExternCharLayout(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		log_char(char x) `+"`"+`extern("test_char");
		main() { }
	`)
	codegentest.AssertContains(t, ir, "%promise_char_v = type { i8*, %promise_char_i*, i32 }")
	codegentest.AssertContains(t, ir, "declare void @test_char(i8*")
}

func TestExternUintLayout(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		log_uint(uint x) `+"`"+`extern("test_uint");
		main() { }
	`)
	codegentest.AssertContains(t, ir, "%promise_uint_v = type { i8*, %promise_uint_i*, i64 }")
	codegentest.AssertContains(t, ir, "declare void @test_uint(i8*")
}

// --- Header generation: return types and zero-param ---

func TestHeaderExternReturnType(t *testing.T) {
	result := codegentest.CompileResult(t, `
		get_val() int `+"`"+`extern("test_get_val");
		main() { x := get_val(); }
	`)

	var buf bytes.Buffer
	if err := codegen.GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// Return type uses sret: void return with first param as result pointer
	codegentest.AssertContains(t, header, "void test_get_val(promise_int_v *sret);")
}

func TestHeaderExternZeroParams(t *testing.T) {
	result := codegentest.CompileResult(t, `
		do_nothing() `+"`"+`extern("test_noop");
		main() { do_nothing(); }
	`)

	var buf bytes.Buffer
	if err := codegen.GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// Zero-param void functions should have (void) in C
	codegentest.AssertContains(t, header, "void test_noop(void);")
}

func TestHeaderExternMultipleTypes(t *testing.T) {
	// Externs only declared (not called) since sema doesn't allow implicit narrowing
	result := codegentest.CompileResult(t, `
		log_i32(i32 x) `+"`"+`extern("test_log_i32");
		log_bool(bool x) `+"`"+`extern("test_log_bool");
		log_f32(f32 x) `+"`"+`extern("test_log_f32");
		main() { }
	`)

	var buf bytes.Buffer
	if err := codegen.GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// bool layout: raw is uint8_t
	codegentest.AssertContains(t, header, "typedef struct { } promise_bool_t;")
	codegentest.AssertContains(t, header, "uint8_t              raw;")

	// i32 layout: raw is int32_t
	codegentest.AssertContains(t, header, "typedef struct { } promise_i32_t;")
	codegentest.AssertContains(t, header, "int32_t              raw;")

	// f32 layout: raw is float
	codegentest.AssertContains(t, header, "typedef struct { } promise_f32_t;")
	codegentest.AssertContains(t, header, "float                raw;")

	// Function declarations: all params passed by pointer
	codegentest.AssertContains(t, header, "void test_log_i32(promise_i32_v *x);")
	codegentest.AssertContains(t, header, "void test_log_bool(promise_bool_v *x);")
	codegentest.AssertContains(t, header, "void test_log_f32(promise_f32_v *x);")
}

// --- Ref param tests (shared & and mutable ~) ---

func TestExternSharedRefParam(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		modify(int& x) `+"`"+`extern("test_modify");
		main() { }
	`)
	// Shared ref param should be a pointer to the value struct
	codegentest.AssertContains(t, ir, "declare void @test_modify(%promise_int_v*")
}

func TestExternMutRefParam(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		update(int ~x) `+"`"+`extern("test_update");
		main() { }
	`)
	// Mutable ref param should be a pointer to the value struct
	codegentest.AssertContains(t, ir, "declare void @test_update(%promise_int_v*")
}

func TestHeaderExternSharedRefParam(t *testing.T) {
	result := codegentest.CompileResult(t, `
		modify(int& x) `+"`"+`extern("test_modify");
		main() { }
	`)

	var buf bytes.Buffer
	if err := codegen.GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// Shared ref param should be pointer in C header
	codegentest.AssertContains(t, header, "void test_modify(promise_int_v *x);")
}

func TestHeaderExternMutRefParam(t *testing.T) {
	result := codegentest.CompileResult(t, `
		update(int ~x) `+"`"+`extern("test_update");
		main() { }
	`)

	var buf bytes.Buffer
	if err := codegen.GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// Mutable ref param should be pointer in C header
	codegentest.AssertContains(t, header, "void test_update(promise_int_v *x);")
}

func TestStringExternPacking(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		print_string(string s) `+"`"+`extern("promise_print_string");
		main() { print_string("hello"); }
	`)
	// Bitcast i8* to promise_string_i*
	codegentest.AssertContains(t, ir, "bitcast i8* %")
	// Insert into value struct
	codegentest.AssertContains(t, ir, "insertvalue %promise_string_v")
}

func TestStringExternReturn(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		get_greeting() string `+"`"+`extern("promise_get_greeting");
		main() { s := get_greeting(); }
	`)
	// Extern returns promise_string_v
	codegentest.AssertContains(t, ir, "define i32 @main(i32 %argc, i8** %argv)")
	// Unpack: extractvalue + bitcast back to i8*
	codegentest.AssertContains(t, ir, "extractvalue %promise_string_v")
	codegentest.AssertContains(t, ir, "bitcast %promise_string_i*")
}

func TestUserTypeExternPacking(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Dog { int age; }
		print_dog(Dog d) `+"`"+`extern("print_dog");
		main() {
			d := Dog(age: 3);
			print_dog(d);
		}
	`)
	// Should pack into value struct
	codegentest.AssertContains(t, ir, "insertvalue %promise_Dog_v")
}

func TestUserTypeExternUnpacking(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Dog { int age; }
		get_dog() Dog `+"`"+`extern("get_dog");
		main() {
			d := get_dog();
		}
	`)
	// Extern uses sret for struct return
	codegentest.AssertContains(t, ir, "declare void @get_dog(i8* %sret)")
	// Unpack: load from sret alloca, extractvalue field 1 + bitcast back to i8*
	codegentest.AssertContains(t, ir, "extractvalue %promise_Dog_v")
	codegentest.AssertContains(t, ir, "bitcast %promise_Dog_i*")
}

func TestEnumExternPacking(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Color { Red, Green, Blue }
		print_color(Color c) `+"`"+`extern("print_color");
		test() {
			Color c = Color.Green;
			print_color(c);
		}
		main() { }
	`)
	// Should pack into value struct
	codegentest.AssertContains(t, ir, "insertvalue %promise_Color_v")
	// Extern declaration: param passed by pointer
	codegentest.AssertContains(t, ir, "declare void @print_color(i8*")
}

func TestEnumExternUnpacking(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Color { Red, Green, Blue }
		get_color() Color `+"`"+`extern("get_color");
		test() {
			Color c = get_color();
		}
		main() { }
	`)
	// Extern uses sret for struct return
	codegentest.AssertContains(t, ir, "declare void @get_color(i8* %sret)")
	// Should unpack via extractvalue after loading from sret
	codegentest.AssertContains(t, ir, "extractvalue %promise_Color_v")
}

func TestEnumDataExternPacking(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Shape { Circle(f64 radius), Rect(f64 w, f64 h) }
		send_shape(Shape s) `+"`"+`extern("send_shape");
		test() {
			Shape s = Shape.Circle(3.14);
			send_shape(s);
		}
		main() { }
	`)
	// Data enum packing: extractvalue tag and data from internal struct
	codegentest.AssertContains(t, ir, "extractvalue %promise_Shape_enum")
	// Pack into value struct
	codegentest.AssertContains(t, ir, "insertvalue %promise_Shape_v")
	// Extern declaration: param passed by pointer
	codegentest.AssertContains(t, ir, "declare void @send_shape(i8*")
}

func TestEnumDataExternUnpacking(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Shape { Circle(f64 radius), Rect(f64 w, f64 h) }
		get_shape() Shape `+"`"+`extern("get_shape");
		test() {
			Shape s = get_shape();
		}
		main() { }
	`)
	// Data enum unpacking: sret + extractvalue from value struct, build internal struct
	codegentest.AssertContains(t, ir, "declare void @get_shape(i8* %sret)")
	codegentest.AssertContains(t, ir, "extractvalue %promise_Shape_v")
	codegentest.AssertContains(t, ir, "insertvalue %promise_Shape_enum")
}

// T0848: casting to a borrow target type (`x as! T&`) used to panic codegen with
// "unsupported cast target type *ast.SharedRefTypeRef" — the target-type switch
// had no ref case. genCastExpr now peels the ref to the underlying named type and
// runs the same RTTI cast, so this compiles and emits the normal promise_type_is
// query plus the `as!` force-cast panic block.
func TestCastToBorrowTarget(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Shape { string name; }
		type Circle is Shape { f64 radius; }
		borrow_return(Shape s) Circle & {
			return s as! Circle &;
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "call i32 @promise_type_is(")
	codegentest.AssertContains(t, ir, "cast.panic")
}

// B0323: Failable call result used as index target must be unwrapped.
func TestAutoPropagateInIndexTarget(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		bar!() int[] { return [1, 2, 3]; }
		wrapper!() int {
			return bar()[0];
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "auto.ok")
}

func TestHostTargetTriple(t *testing.T) {
	triple := codegen.HostTargetTriple()
	if triple == "" {
		t.Fatal("HostTargetTriple returned empty string")
	}
	// Should contain a known arch
	if !strings.Contains(triple, "arm64") && !strings.Contains(triple, "x86_64") && !strings.Contains(triple, "aarch64") {
		t.Errorf("unexpected target triple: %s", triple)
	}
}

func TestStdExternRegistration(t *testing.T) {
	// Std externs should be callable via std.X() and normal call
	ir := codegentest.GenerateIRWithStd(t,
		`_do_thing(int x) `+"`"+`extern("c_do_thing");`,
		`main() { _do_thing(42); }`,
	)
	// The C function should be declared
	codegentest.AssertContains(t, ir, "declare void @c_do_thing")
}

func TestStdExternDedupWithUserExtern(t *testing.T) {
	// User extern with same C name as std extern should share the IR declaration
	ir := codegentest.GenerateIRWithStd(t,
		`_std_thing(int x) `+"`"+`extern("c_shared_fn");`,
		`
		my_thing(int x) `+"`"+`extern("c_shared_fn");
		main() { my_thing(42); }
		`,
	)
	// Only one C declaration (not two)
	count := strings.Count(ir, "declare void @c_shared_fn")
	if count != 1 {
		t.Errorf("expected 1 declaration of @c_shared_fn, got %d", count)
	}
}

func TestOptionalExternSret(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		get_val(string name) string? `+"`"+`extern("promise_get_val");
		main() {
			string? v = get_val("key");
		}
	`)
	// Optional extern uses sret with {i1, T} struct
	codegentest.AssertContains(t, ir, "declare void @promise_get_val(")
	// Caller allocates sret and loads result
	codegentest.AssertContains(t, ir, "call void @promise_get_val(")
}

func TestFailableExternSret(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		get_cwd!() string `+"`"+`extern("promise_get_cwd");
		main() {
			string s = get_cwd()?!;
		}
	`)
	// Failable extern uses sret with {i1, T, i8*} struct
	codegentest.AssertContains(t, ir, "declare void @promise_get_cwd(")
	// Caller allocates sret and loads result
	codegentest.AssertContains(t, ir, "call void @promise_get_cwd(")
}

// T1660: the canonical-ABI helpers in wasm_alloc.o are plain C functions, so
// every `extern naming one must lower to the raw C ABI — scalars by value,
// string as its bare instance pointer, results returned directly. Before this
// fix they took the value-struct bridge path (sret-first, every scalar as an i8*
// to its value struct), which wasm-ld resolved to a trapping stub for the shapes
// it could see and linked *silently wrong* for the rest (cabi_store_i32's
// (i8*,i8*) is indistinguishable from (i32,i32) on wasm32).
//
// The ABI belongs to the symbol, not the target, so the same lowering applies on
// every target; cabiHelperDecls is asserted for wasm32-wasi, wasm32-web and the
// host below.
var cabiHelperDecls = []string{
	"declare i32 @cabi_retarea_ptr()",
	"declare i32 @cabi_load_i32(i32 %ptr)",
	"declare i64 @cabi_load_i64(i32 %ptr)",
	"declare float @cabi_load_f32(i32 %ptr)",
	"declare double @cabi_load_f64(i32 %ptr)",
	"declare void @cabi_store_i32(i32 %ptr, i32 %val)",
	"declare void @cabi_store_i64(i32 %ptr, i64 %val)",
	"declare void @cabi_store_f32(i32 %ptr, float %val)",
	"declare void @cabi_store_f64(i32 %ptr, double %val)",
	"declare i32 @cabi_string_data(i8* %s)",
	"declare i32 @cabi_string_len(i8* %s)",
	"declare i8* @cabi_string_from(i32 %ptr, i32 %len)",
	"declare i32 @cabi_vector_data(i8* %v)",
	"declare i32 @cabi_vector_len(i8* %v)",
	"declare i8* @cabi_vector_from(i32 %ptr, i32 %len, i32 %elem_size)",
	"declare i32 @cabi_realloc(i32 %ptr, i32 %old_size, i32 %align, i32 %new_size)",
}

// cabiHelperSource declares and calls every helper in rawABIExternSymbols, so
// each one is actually emitted (an uncalled extern is never declared).
const cabiHelperSource = "" +
	"_cabi_retarea_ptr() i32 `extern(\"cabi_retarea_ptr\");\n" +
	"_cabi_load_i32(i32 ptr) i32 `extern(\"cabi_load_i32\");\n" +
	"_cabi_load_i64(i32 ptr) i64 `extern(\"cabi_load_i64\");\n" +
	"_cabi_load_f32(i32 ptr) f32 `extern(\"cabi_load_f32\");\n" +
	"_cabi_load_f64(i32 ptr) f64 `extern(\"cabi_load_f64\");\n" +
	"_cabi_store_i32(i32 ptr, i32 val) `extern(\"cabi_store_i32\");\n" +
	"_cabi_store_i64(i32 ptr, i64 val) `extern(\"cabi_store_i64\");\n" +
	"_cabi_store_f32(i32 ptr, f32 val) `extern(\"cabi_store_f32\");\n" +
	"_cabi_store_f64(i32 ptr, f64 val) `extern(\"cabi_store_f64\");\n" +
	"_cabi_string_data(string s) i32 `extern(\"cabi_string_data\");\n" +
	"_cabi_string_len(string s) i32 `extern(\"cabi_string_len\");\n" +
	"_cabi_string_from(i32 ptr, i32 len) string `extern(\"cabi_string_from\");\n" +
	"_cabi_vector_data_u8(u8[] v) i32 `extern(\"cabi_vector_data\");\n" +
	"_cabi_vector_len_u8(u8[] v) i32 `extern(\"cabi_vector_len\");\n" +
	"_cabi_vector_from_u8(i32 ptr, i32 len, i32 elem_size) u8[] `extern(\"cabi_vector_from\");\n" +
	"_cabi_realloc(i32 ptr, i32 old_size, i32 align, i32 new_size) i32 `extern(\"cabi_realloc\");\n" +
	"main() {\n" +
	"  area := _cabi_retarea_ptr();\n" +
	"  _cabi_store_i32(area, _cabi_load_i32(area));\n" +
	"  _cabi_store_i64(area, _cabi_load_i64(area));\n" +
	"  _cabi_store_f32(area, _cabi_load_f32(area));\n" +
	"  _cabi_store_f64(area, _cabi_load_f64(area));\n" +
	"  s := _cabi_string_from(_cabi_string_data(\"x\"), _cabi_string_len(\"x\"));\n" +
	"  u8[] v = _cabi_vector_from_u8(0i32, 0i32, 1i32);\n" +
	"  n := _cabi_vector_data_u8(v) + _cabi_vector_len_u8(v);\n" +
	"  m := _cabi_realloc(0i32, 0i32, 1i32, 4i32);\n" +
	"}\n"

func assertCabiHelpersRaw(t *testing.T, ir string) {
	t.Helper()
	for _, want := range cabiHelperDecls {
		codegentest.AssertContains(t, ir, want)
	}
	// No helper may keep the value-struct bridge shape.
	codegentest.AssertNotContains(t, ir, "@cabi_load_i32(i8* %sret")
	codegentest.AssertNotContains(t, ir, "@cabi_string_from(i8* %sret")
	codegentest.AssertNotContains(t, ir, "@cabi_store_i32(i8* %ptr")
}

func TestCabiHelpersUseRawCABI(t *testing.T) {
	assertCabiHelpersRaw(t, codegentest.GenerateIRForTarget(t, cabiHelperSource, "wasm32-wasi"))
}

func TestCabiHelpersUseRawCABIOnWasmWeb(t *testing.T) {
	assertCabiHelpersRaw(t, codegentest.GenerateIRForTarget(t, cabiHelperSource, "wasm32-web"))
}

// The registry is keyed on the symbol, not the target: an extern naming a
// wasm_alloc.o helper lowers the same way everywhere, so a host build fails at
// link time (symbol absent) rather than silently producing a different ABI.
func TestCabiHelpersUseRawCABIOnHost(t *testing.T) {
	assertCabiHelpersRaw(t, codegentest.GenerateIR(t, cabiHelperSource))
}

// The raw-ABI path must stay narrow: a plain extern whose symbol is not a
// wasm_alloc.o helper keeps the value-struct bridge ABI on WASM (the T0283
// scoping that the codegen-synthesized platform layer depends on).
func TestNonCabiExternKeepsBridgeABIOnWasm(t *testing.T) {
	ir := codegentest.GenerateIRForTarget(t, `
		_cabi_lookalike(i32 ptr) i32 `+"`extern(\"cabi_lookalike\")"+`;
		main() { x := _cabi_lookalike(0i32); }
	`, "wasm32-wasi")
	codegentest.AssertContains(t, ir, "declare void @cabi_lookalike(i8* %sret, i8* %ptr)")
}

// The raw-ABI path applies only to a declaration that actually has a raw C form.
// `extern is writable in ordinary source, so any signature at all can name a
// canonical-ABI symbol; codegen must lower it, not crash. Each case below keeps
// the value-struct bridge ABI, and the wasm-ld signature check then reports the
// disagreement with wasm_alloc.o by name (T1660).
func TestCabiSymbolWithNoRawFormKeepsBridgeABI(t *testing.T) {
	// A user-type parameter has no raw C form.
	ir := codegentest.GenerateIRForTarget(t, `
		type Thing { int x; }
		_load(Thing t) i32 `+"`extern(\"cabi_load_i32\")"+`;
		main() { th := Thing(x: 1); n := _load(th); }
	`, "wasm32-wasi")
	codegentest.AssertContains(t, ir, "declare void @cabi_load_i32(i8* %sret, i8* %t)")
}

// An optional result is {i1, T}, which C never declares. It matters that this
// falls back rather than matching on the payload: extractNamed does not unwrap
// Optional, so a lookupLayout miss must read as "no raw form" — treating it as
// the bare payload scalar would drop the presence bit silently.
func TestOptionalCabiResultKeepsSret(t *testing.T) {
	ir := codegentest.GenerateIRForTarget(t, `
		_load(i32 ptr) i32? `+"`extern(\"cabi_load_i32\")"+`;
		main() { n := _load(0i32); }
	`, "wasm32-wasi")
	codegentest.AssertContains(t, ir, "declare void @cabi_load_i32(i8* %sret, i8* %ptr)")
}

func TestFailableCabiSymbolKeepsSret(t *testing.T) {
	// A failable result is Promise's own {i1, T, i8*} struct, which C never
	// declares — so sret stays, exactly as for any other failable extern.
	ir := codegentest.GenerateIRForTarget(t, `
		_load!(i32 ptr) i32 `+"`extern(\"cabi_load_i64\")"+`;
		main() {}
	`, "wasm32-wasi")
	codegentest.AssertContains(t, ir, "declare void @cabi_load_i64(i8* %sret, i8* %ptr)")
}

func TestWasmImportWinsOverRawABIRegistry(t *testing.T) {
	// `wasm_import says the symbol is a *host* import, not the statically-linked
	// wasm_alloc.o helper — so the canonical import convention applies and the
	// registry does not.
	ir := codegentest.GenerateIRForTarget(t, `
		_load(i32 ptr) i32 `+"`extern(\"cabi_load_f32\") `wasm_import(\"env\", \"cabi_load_f32\") `target(wasm)"+`;
		main() { n := _load(0i32); }
	`, "wasm32-wasi")
	codegentest.AssertContains(t, ir, `declare i32 @cabi_load_f32(i32 %ptr) "wasm-import-module"="env"`)
}

// A `&` parameter must never take the raw path, even when its element type is a
// scalar or a string. extractNamed unwraps SharedRef/MutRef, so `i32&` looks like
// a plain i32 to a layout-kind test — fitsRawABI has to reject the ref itself.
//
// This is the one fallback whose absence wasm-ld could not catch. Dropping the
// guard declares a hybrid — raw direct return, but the parameter still a typed
// pointer from the ref branch above it — and genExternCall then passes the
// pointer. On wasm32 that is (i32) -> i32 on both sides, matching C's signature
// exactly, so the link is clean and cabi_load_i32 silently dereferences a
// pointer-to-value-struct as if it were the caller's address (T1660).
func TestRefParamOnCabiSymbolKeepsBridgeABI(t *testing.T) {
	ir := codegentest.GenerateIRForTarget(t, `
		_load(i32& p) i32 `+"`extern(\"cabi_load_i32\")"+`;
		_len(string& s) i32 `+"`extern(\"cabi_string_len\")"+`;
		main() {
		  i32 v = 7i32;
		  n := _load(v);
		  string t = "ab";
		  m := _len(t);
		}
	`, "wasm32-wasi")
	// Typed pointer to the value struct, and sret back — the bridge ABI intact.
	codegentest.AssertContains(t, ir, "declare void @cabi_load_i32(i8* %sret, %promise_i32_v* %p)")
	codegentest.AssertContains(t, ir, "declare void @cabi_string_len(i8* %sret, %promise_string_v* %s)")
	// The hybrid shape a missing guard produces must not appear.
	codegentest.AssertNotContains(t, ir, "declare i32 @cabi_load_i32(%promise_i32_v*")
	codegentest.AssertNotContains(t, ir, "declare i32 @cabi_string_len(%promise_string_v*")
}
