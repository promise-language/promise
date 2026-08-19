package codegen

import (
	"bytes"
	"strings"
	"testing"
)

// --- Extern print (struct-based ABI) ---

func TestPrintStringExtern(t *testing.T) {
	ir := generateIR(t, `
		print_s(string s) `+"`"+`extern("promise_print_string");
		main() { print_s("hello"); }
	`)
	assertContains(t, ir, "%promise_string_v = type")
	assertContains(t, ir, "define void @promise_print_string(i8*")
}

// --- Extern architecture tests ---

func TestExternCustomCName(t *testing.T) {
	ir := generateIR(t, `
		log_value(int x) `+"`"+`extern("my_log_int");
		main() { log_value(99); }
	`)
	assertContains(t, ir, "declare void @my_log_int(i8*")
	assertContains(t, ir, "call void @my_log_int(i8*")
}

func TestExternDefaultCName(t *testing.T) {
	ir := generateIR(t, `
		do_thing(int x) `+"`"+`extern;
		main() { do_thing(1); }
	`)
	assertContains(t, ir, "declare void @promise_do_thing(i8*")
}

// --- wall-clock (realtime) extern body tests (T0962) ---

// The time module's promise_wallclock extern gets its body from defineTimeBodies.
// On POSIX it reads CLOCK_REALTIME (id 0 on both Linux and macOS), distinct from
// the monotonic nanotime read (id 1 on Linux / 6 on macOS).
func TestWallclockExternBodyPosix(t *testing.T) {
	ir := generateIRForTarget(t, `
		_wallclock() int `+"`extern(\"promise_wallclock\")"+`;
		main() { int _x = _wallclock(); }
	`, "x86_64-unknown-linux-gnu")
	assertContains(t, ir, "define void @promise_wallclock(i8* %sret)")
	// CLOCK_REALTIME is 0 — the realtime clock, not the monotonic one.
	assertContains(t, ir, "call i32 @clock_gettime(i32 0,")
}

// On wasm32-wasi the body reads CLOCK_REALTIME (clockid 0) via the WASI
// clock_time_get import — the same import the monotonic clock uses (clockid 1),
// distinct only in the clockid argument (T1067).
func TestWallclockExternBodyWasiRealtime(t *testing.T) {
	ir := generateIRForTarget(t, `
		_wallclock() int `+"`extern(\"promise_wallclock\")"+`;
		main() { int _x = _wallclock(); }
	`, "wasm32-wasi")
	assertContains(t, ir, "define void @promise_wallclock(i8* %sret)")
	// Realtime clockid is 0, passed to the WASI clock_time_get import.
	assertContains(t, ir, "call i32 @clock_time_get(i32 0,")
	if strings.Contains(ir, "@clock_gettime") {
		t.Errorf("wasm32-wasi wallclock body must not call clock_gettime\ngot:\n%s", ir)
	}
}

// On wasm32-web there is no guaranteed realtime source, so the body returns a
// constant 0 — no WASI clock_time_get import and no POSIX clock_gettime (T1067).
func TestWallclockExternBodyWasmWebReturnsZero(t *testing.T) {
	ir := generateIRForTarget(t, `
		_wallclock() int `+"`extern(\"promise_wallclock\")"+`;
		main() { int _x = _wallclock(); }
	`, "wasm32-web")
	assertContains(t, ir, "define void @promise_wallclock(i8* %sret)")
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
	ir := generateIRForTarget(t, `
		_wallclock() int `+"`extern(\"promise_wallclock\")"+`;
		main() { int _x = _wallclock(); }
	`, "x86_64-pc-windows-msvc")
	assertContains(t, ir, "define void @promise_wallclock(i8* %sret)")
	assertContains(t, ir, "@GetSystemTimePreciseAsFileTime")
	// Unix-epoch shift constant: 116444736000000000 ticks from 1601 → 1970.
	assertContains(t, ir, "116444736000000000")
}

func TestWasmExternDirectReturn(t *testing.T) {
	ir := generateIRForTarget(t, `
		_sched_yield() i32 `+"`extern(\"__wasi_sched_yield\") `wasm_import(\"wasi_snapshot_preview1\", \"sched_yield\") `target(wasm)"+`;
		main() {}
	`, "wasm32-wasi")
	assertContains(t, ir, "declare i32 @__wasi_sched_yield()")
}

func TestWasmExternDirectParams(t *testing.T) {
	ir := generateIRForTarget(t, `
		_fd_close(i32 fd) `+"`extern(\"fd_close\") `wasm_import(\"wasi_snapshot_preview1\", \"fd_close\") `target(wasm)"+`;
		main() {}
	`, "wasm32-wasi")
	assertContains(t, ir, "declare void @fd_close(i32 %fd)")
}

func TestWasmExternDirectReturnWithParams(t *testing.T) {
	ir := generateIRForTarget(t, `
		_fd_read(i32 fd, i32 iovs, i32 iovs_len, i32 nwritten) i32 `+"`extern(\"fd_read\") `wasm_import(\"wasi_snapshot_preview1\", \"fd_read\") `target(wasm)"+`;
		main() {}
	`, "wasm32-wasi")
	assertContains(t, ir, "declare i32 @fd_read(i32 %fd, i32 %iovs, i32 %iovs_len, i32 %nwritten)")
}

func TestWasmExternDirectCall(t *testing.T) {
	ir := generateIRForTarget(t, `
		_get() int `+"`extern(\"test_get\") `wasm_import(\"env\", \"test_get\") `target(wasm)"+`;
		main() { x := _get(); }
	`, "wasm32-wasi")
	assertContains(t, ir, "declare i64 @test_get()")
	assertContains(t, ir, "call i64 @test_get()")
}

func TestWasmExternDirectCallWithParams(t *testing.T) {
	ir := generateIRForTarget(t, `
		_add(int a, int b) int `+"`extern(\"test_add\") `wasm_import(\"env\", \"test_add\") `target(wasm)"+`;
		main() { x := _add(1, 2); }
	`, "wasm32-wasi")
	assertContains(t, ir, "declare i64 @test_add(i64 %a, i64 %b)")
	assertContains(t, ir, "call i64 @test_add(i64")
}

func TestWasmExternNativeUnchanged(t *testing.T) {
	// Native targets still use sret/i8* for the same types
	ir := generateIR(t, `
		get_value() int `+"`"+`extern("native_get");
		use_value(int x) `+"`"+`extern("native_use");
		main() { use_value(1); x := get_value(); }
	`)
	assertContains(t, ir, "declare void @native_get(i8* %sret)")
	assertContains(t, ir, "declare void @native_use(i8* %x)")
}

func TestWasmExternBoolReturn(t *testing.T) {
	ir := generateIRForTarget(t, `
		_check() bool `+"`extern(\"test_check\") `wasm_import(\"env\", \"test_check\") `target(wasm)"+`;
		main() { x := _check(); }
	`, "wasm32-wasi")
	// Bool (i1) should use direct return on WASM, not sret
	assertContains(t, ir, "declare i1 @test_check()")
	assertContains(t, ir, "call i1 @test_check()")
}

func TestWasmExternF64Param(t *testing.T) {
	ir := generateIRForTarget(t, `
		_set(f64 val) `+"`extern(\"test_set\") `wasm_import(\"env\", \"test_set\") `target(wasm)"+`;
		main() { _set(3.14); }
	`, "wasm32-wasi")
	// f64 (double) should use direct param on WASM
	assertContains(t, ir, "declare void @test_set(double %val)")
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
	ir := generateIRForTarget(t, `
		take_string(string s) `+"`extern(\"take_string\") `wasm_import(\"dbg\", \"take_string\") `target(web)"+`;
		main() { take_string("hello"); }
	`, "wasm32-web")
	assertContains(t, ir, `declare void @take_string(i8* %s_ptr, i32 %s_len) `)
	assertContains(t, ir, `"wasm-import-name"="take_string"`)
	assertContains(t, ir, "call void @take_string(i8*")
}

// T1506: the flattening must be scoped to wasm_import externs only — a plain
// (non-wasm_import) native extern taking a string param keeps passing the
// existing boxed value-struct pointer, matching the native C ABI other
// native extern callers (e.g. cabi_string_data in wasm_alloc.c) already rely on.
func TestWasmExternStringParamUnchangedForNonWasmImport(t *testing.T) {
	ir := generateIR(t, `
		_take(string s) `+"`extern(\"native_take\")"+`;
		main() { _take("hello"); }
	`)
	assertContains(t, ir, "declare void @native_take(i8* %s)")
}

func TestWasmExternFailableStillSret(t *testing.T) {
	ir := generateIRForTarget(t, `
		_open!(i32 fd) i32 `+"`extern(\"test_open\") `wasm_import(\"env\", \"test_open\") `target(wasm)"+`;
		main() {}
	`, "wasm32-wasi")
	// Failable externs always use sret, even on WASM
	assertContains(t, ir, "declare void @test_open(i8* %sret")
}

// T0315: wasm32-web targets must export @_initialize (the JS/Node entry point)
// rather than @_start (the WASI Command convention).
func TestWasmWebEmitsInitialize(t *testing.T) {
	ir := generateIRForTarget(t, `
		main() { print_line("hi"); }
	`, "wasm32-web")
	assertContains(t, ir, "define void @_initialize()")
	assertNotContains(t, ir, "define void @_start()")
}

// wasm32-wasi keeps the existing @_start export.
func TestWasmWasiEmitsStart(t *testing.T) {
	ir := generateIRForTarget(t, `
		main() { print_line("hi"); }
	`, "wasm32-wasi")
	assertContains(t, ir, "define void @_start()")
	assertNotContains(t, ir, "define void @_initialize()")
}

func TestExternMultipleParams(t *testing.T) {
	ir := generateIR(t, `
		add_ext(int a, int b) `+"`"+`extern("test_add");
		main() { add_ext(1, 2); }
	`)
	assertContains(t, ir, "declare void @test_add(i8* %a, i8* %b)")
	assertContains(t, ir, "call void @test_add")
}

func TestExternReturnValue(t *testing.T) {
	ir := generateIR(t, `
		get_value() int `+"`"+`extern("test_get");
		main() { x := get_value(); }
	`)
	// sret: struct return becomes void with first param as result pointer
	assertContains(t, ir, "declare void @test_get(i8* %sret)")
	// Return value should be loaded from sret alloca and unpacked
	assertContains(t, ir, "extractvalue %promise_int_v")
}

func TestExternStructTypeDefs(t *testing.T) {
	ir := generateIR(t, `
		use_int(int x) `+"`"+`extern("test_use_int");
		main() { use_int(42); }
	`)
	// All four struct types should be defined
	assertContains(t, ir, "%promise_int_t = type {}")
	assertContains(t, ir, "%promise_int_m = type { %promise_int_t* }")
	assertContains(t, ir, "%promise_int_i = type { %promise_int_m* }")
	assertContains(t, ir, "%promise_int_v = type { i8*, %promise_int_i*, i64 }")
}

// --- Primitive type layout coverage ---
// These tests verify that layout computation and extern declarations work
// for all primitive types. Externs are declared but not called since sema
// doesn't allow implicit narrowing from int/f64 literals to narrow types.

func TestExternI8Layout(t *testing.T) {
	ir := generateIR(t, `
		log_i8(i8 x) `+"`"+`extern("test_i8");
		main() { }
	`)
	assertContains(t, ir, "%promise_i8_v = type { i8*, %promise_i8_i*, i8 }")
	assertContains(t, ir, "%promise_i8_i = type { %promise_i8_m* }")
	assertContains(t, ir, "%promise_i8_m = type { %promise_i8_t* }")
	assertContains(t, ir, "%promise_i8_t = type {}")
	assertContains(t, ir, "declare void @test_i8(i8*")
}

func TestExternI16Layout(t *testing.T) {
	ir := generateIR(t, `
		log_i16(i16 x) `+"`"+`extern("test_i16");
		main() { }
	`)
	assertContains(t, ir, "%promise_i16_v = type { i8*, %promise_i16_i*, i16 }")
	assertContains(t, ir, "declare void @test_i16(i8*")
}

func TestExternI32Layout(t *testing.T) {
	ir := generateIR(t, `
		log_i32(i32 x) `+"`"+`extern("test_i32");
		main() { }
	`)
	assertContains(t, ir, "%promise_i32_v = type { i8*, %promise_i32_i*, i32 }")
	assertContains(t, ir, "declare void @test_i32(i8*")
}

func TestExternU8Layout(t *testing.T) {
	ir := generateIR(t, `
		log_u8(u8 x) `+"`"+`extern("test_u8");
		main() { }
	`)
	assertContains(t, ir, "%promise_u8_v = type { i8*, %promise_u8_i*, i8 }")
	assertContains(t, ir, "declare void @test_u8(i8*")
}

func TestExternU16Layout(t *testing.T) {
	ir := generateIR(t, `
		log_u16(u16 x) `+"`"+`extern("test_u16");
		main() { }
	`)
	assertContains(t, ir, "%promise_u16_v = type { i8*, %promise_u16_i*, i16 }")
	assertContains(t, ir, "declare void @test_u16(i8*")
}

func TestExternU32Layout(t *testing.T) {
	ir := generateIR(t, `
		log_u32(u32 x) `+"`"+`extern("test_u32");
		main() { }
	`)
	assertContains(t, ir, "%promise_u32_v = type { i8*, %promise_u32_i*, i32 }")
	assertContains(t, ir, "declare void @test_u32(i8*")
}

func TestExternU64Layout(t *testing.T) {
	ir := generateIR(t, `
		log_u64(u64 x) `+"`"+`extern("test_u64");
		main() { }
	`)
	assertContains(t, ir, "%promise_u64_v = type { i8*, %promise_u64_i*, i64 }")
	assertContains(t, ir, "declare void @test_u64(i8*")
}

func TestExternI64Layout(t *testing.T) {
	ir := generateIR(t, `
		log_i64(i64 x) `+"`"+`extern("test_i64");
		main() { }
	`)
	assertContains(t, ir, "%promise_i64_v = type { i8*, %promise_i64_i*, i64 }")
	assertContains(t, ir, "declare void @test_i64(i8*")
}

func TestExternF32Layout(t *testing.T) {
	ir := generateIR(t, `
		log_f32(f32 x) `+"`"+`extern("test_f32");
		main() { }
	`)
	assertContains(t, ir, "%promise_f32_v = type { i8*, %promise_f32_i*, float }")
	assertContains(t, ir, "declare void @test_f32(i8*")
}

func TestExternCharLayout(t *testing.T) {
	ir := generateIR(t, `
		log_char(char x) `+"`"+`extern("test_char");
		main() { }
	`)
	assertContains(t, ir, "%promise_char_v = type { i8*, %promise_char_i*, i32 }")
	assertContains(t, ir, "declare void @test_char(i8*")
}

func TestExternUintLayout(t *testing.T) {
	ir := generateIR(t, `
		log_uint(uint x) `+"`"+`extern("test_uint");
		main() { }
	`)
	assertContains(t, ir, "%promise_uint_v = type { i8*, %promise_uint_i*, i64 }")
	assertContains(t, ir, "declare void @test_uint(i8*")
}

// --- Header generation: return types and zero-param ---

func TestHeaderExternReturnType(t *testing.T) {
	result := compileResult(t, `
		get_val() int `+"`"+`extern("test_get_val");
		main() { x := get_val(); }
	`)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// Return type uses sret: void return with first param as result pointer
	assertContains(t, header, "void test_get_val(promise_int_v *sret);")
}

func TestHeaderExternZeroParams(t *testing.T) {
	result := compileResult(t, `
		do_nothing() `+"`"+`extern("test_noop");
		main() { do_nothing(); }
	`)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// Zero-param void functions should have (void) in C
	assertContains(t, header, "void test_noop(void);")
}

func TestHeaderExternMultipleTypes(t *testing.T) {
	// Externs only declared (not called) since sema doesn't allow implicit narrowing
	result := compileResult(t, `
		log_i32(i32 x) `+"`"+`extern("test_log_i32");
		log_bool(bool x) `+"`"+`extern("test_log_bool");
		log_f32(f32 x) `+"`"+`extern("test_log_f32");
		main() { }
	`)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// bool layout: raw is uint8_t
	assertContains(t, header, "typedef struct { } promise_bool_t;")
	assertContains(t, header, "uint8_t              raw;")

	// i32 layout: raw is int32_t
	assertContains(t, header, "typedef struct { } promise_i32_t;")
	assertContains(t, header, "int32_t              raw;")

	// f32 layout: raw is float
	assertContains(t, header, "typedef struct { } promise_f32_t;")
	assertContains(t, header, "float                raw;")

	// Function declarations: all params passed by pointer
	assertContains(t, header, "void test_log_i32(promise_i32_v *x);")
	assertContains(t, header, "void test_log_bool(promise_bool_v *x);")
	assertContains(t, header, "void test_log_f32(promise_f32_v *x);")
}

// --- Ref param tests (shared & and mutable ~) ---

func TestExternSharedRefParam(t *testing.T) {
	ir := generateIR(t, `
		modify(int& x) `+"`"+`extern("test_modify");
		main() { }
	`)
	// Shared ref param should be a pointer to the value struct
	assertContains(t, ir, "declare void @test_modify(%promise_int_v*")
}

func TestExternMutRefParam(t *testing.T) {
	ir := generateIR(t, `
		update(int ~x) `+"`"+`extern("test_update");
		main() { }
	`)
	// Mutable ref param should be a pointer to the value struct
	assertContains(t, ir, "declare void @test_update(%promise_int_v*")
}

func TestHeaderExternSharedRefParam(t *testing.T) {
	result := compileResult(t, `
		modify(int& x) `+"`"+`extern("test_modify");
		main() { }
	`)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// Shared ref param should be pointer in C header
	assertContains(t, header, "void test_modify(promise_int_v *x);")
}

func TestHeaderExternMutRefParam(t *testing.T) {
	result := compileResult(t, `
		update(int ~x) `+"`"+`extern("test_update");
		main() { }
	`)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// Mutable ref param should be pointer in C header
	assertContains(t, header, "void test_update(promise_int_v *x);")
}

func TestStringExternPacking(t *testing.T) {
	ir := generateIR(t, `
		print_string(string s) `+"`"+`extern("promise_print_string");
		main() { print_string("hello"); }
	`)
	// Bitcast i8* to promise_string_i*
	assertContains(t, ir, "bitcast i8* %")
	// Insert into value struct
	assertContains(t, ir, "insertvalue %promise_string_v")
}

func TestStringExternReturn(t *testing.T) {
	ir := generateIR(t, `
		get_greeting() string `+"`"+`extern("promise_get_greeting");
		main() { s := get_greeting(); }
	`)
	// Extern returns promise_string_v
	assertContains(t, ir, "define i32 @main(i32 %argc, i8** %argv)")
	// Unpack: extractvalue + bitcast back to i8*
	assertContains(t, ir, "extractvalue %promise_string_v")
	assertContains(t, ir, "bitcast %promise_string_i*")
}

func TestUserTypeExternPacking(t *testing.T) {
	ir := generateIR(t, `
		type Dog { int age; }
		print_dog(Dog d) `+"`"+`extern("print_dog");
		main() {
			d := Dog(age: 3);
			print_dog(d);
		}
	`)
	// Should pack into value struct
	assertContains(t, ir, "insertvalue %promise_Dog_v")
}

func TestUserTypeExternUnpacking(t *testing.T) {
	ir := generateIR(t, `
		type Dog { int age; }
		get_dog() Dog `+"`"+`extern("get_dog");
		main() {
			d := get_dog();
		}
	`)
	// Extern uses sret for struct return
	assertContains(t, ir, "declare void @get_dog(i8* %sret)")
	// Unpack: load from sret alloca, extractvalue field 1 + bitcast back to i8*
	assertContains(t, ir, "extractvalue %promise_Dog_v")
	assertContains(t, ir, "bitcast %promise_Dog_i*")
}

func TestEnumExternPacking(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		print_color(Color c) `+"`"+`extern("print_color");
		test() {
			Color c = Color.Green;
			print_color(c);
		}
		main() { }
	`)
	// Should pack into value struct
	assertContains(t, ir, "insertvalue %promise_Color_v")
	// Extern declaration: param passed by pointer
	assertContains(t, ir, "declare void @print_color(i8*")
}

func TestEnumExternUnpacking(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		get_color() Color `+"`"+`extern("get_color");
		test() {
			Color c = get_color();
		}
		main() { }
	`)
	// Extern uses sret for struct return
	assertContains(t, ir, "declare void @get_color(i8* %sret)")
	// Should unpack via extractvalue after loading from sret
	assertContains(t, ir, "extractvalue %promise_Color_v")
}

func TestEnumDataExternPacking(t *testing.T) {
	ir := generateIR(t, `
		enum Shape { Circle(f64 radius), Rect(f64 w, f64 h) }
		send_shape(Shape s) `+"`"+`extern("send_shape");
		test() {
			Shape s = Shape.Circle(3.14);
			send_shape(s);
		}
		main() { }
	`)
	// Data enum packing: extractvalue tag and data from internal struct
	assertContains(t, ir, "extractvalue %promise_Shape_enum")
	// Pack into value struct
	assertContains(t, ir, "insertvalue %promise_Shape_v")
	// Extern declaration: param passed by pointer
	assertContains(t, ir, "declare void @send_shape(i8*")
}

func TestEnumDataExternUnpacking(t *testing.T) {
	ir := generateIR(t, `
		enum Shape { Circle(f64 radius), Rect(f64 w, f64 h) }
		get_shape() Shape `+"`"+`extern("get_shape");
		test() {
			Shape s = get_shape();
		}
		main() { }
	`)
	// Data enum unpacking: sret + extractvalue from value struct, build internal struct
	assertContains(t, ir, "declare void @get_shape(i8* %sret)")
	assertContains(t, ir, "extractvalue %promise_Shape_v")
	assertContains(t, ir, "insertvalue %promise_Shape_enum")
}

// T0848: casting to a borrow target type (`x as! T&`) used to panic codegen with
// "unsupported cast target type *ast.SharedRefTypeRef" — the target-type switch
// had no ref case. genCastExpr now peels the ref to the underlying named type and
// runs the same RTTI cast, so this compiles and emits the normal promise_type_is
// query plus the `as!` force-cast panic block.
func TestCastToBorrowTarget(t *testing.T) {
	ir := generateIR(t, `
		type Shape { string name; }
		type Circle is Shape { f64 radius; }
		borrow_return(Shape s) Circle & {
			return s as! Circle &;
		}
		main() { }
	`)
	assertContains(t, ir, "call i32 @promise_type_is(")
	assertContains(t, ir, "cast.panic")
}

// B0323: Failable call result used as index target must be unwrapped.
func TestAutoPropagateInIndexTarget(t *testing.T) {
	ir := generateIR(t, `
		bar!() int[] { return [1, 2, 3]; }
		wrapper!() int {
			return bar()[0];
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

func TestHostTargetTriple(t *testing.T) {
	triple := HostTargetTriple()
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
	ir := generateIRWithStd(t,
		`_do_thing(int x) `+"`"+`extern("c_do_thing");`,
		`main() { _do_thing(42); }`,
	)
	// The C function should be declared
	assertContains(t, ir, "declare void @c_do_thing")
}

func TestStdExternDedupWithUserExtern(t *testing.T) {
	// User extern with same C name as std extern should share the IR declaration
	ir := generateIRWithStd(t,
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
	ir := generateIR(t, `
		get_val(string name) string? `+"`"+`extern("promise_get_val");
		main() {
			string? v = get_val("key");
		}
	`)
	// Optional extern uses sret with {i1, T} struct
	assertContains(t, ir, "declare void @promise_get_val(")
	// Caller allocates sret and loads result
	assertContains(t, ir, "call void @promise_get_val(")
}

func TestFailableExternSret(t *testing.T) {
	ir := generateIR(t, `
		get_cwd!() string `+"`"+`extern("promise_get_cwd");
		main() {
			string s = get_cwd()?!;
		}
	`)
	// Failable extern uses sret with {i1, T, i8*} struct
	assertContains(t, ir, "declare void @promise_get_cwd(")
	// Caller allocates sret and loads result
	assertContains(t, ir, "call void @promise_get_cwd(")
}
