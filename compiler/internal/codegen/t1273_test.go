package codegen

import "testing"

// T1273 — std standard-stream getters (stdin/stdout/stderr) over the
// fd-parameterized PAL primitives. These IR-shape tests pin the two new extern
// bodies (promise_write_bytes over pal_write, promise_read_bytes over
// pal_file_read), the fd baked into each getter, the value->structural boxing
// that hands the getter back as a Writer/Reader, and the regression guard that
// print_string still writes to fd 1.

// TestWriteBytesBody verifies promise_write_bytes is defined over pal_write and
// threads the runtime fd through (not a hardcoded constant).
func TestWriteBytesBody(t *testing.T) {
	ir := generateIR(t, `
		main!() { stdout.write_line("y")?^; }
	`)
	assertContains(t, ir, "define void @promise_write_bytes(i8* %sret, i8* %fd, i8* %buf)")
	// fd is dynamic (extracted from the int param), then pal_write is called.
	assertContainsMatch(t, ir, `@promise_write_bytes\(i8\* %sret.*\n(.*\n)*?.*call i64 @pal_write\(i32 %`)
}

// TestReadBytesBody verifies promise_read_bytes is defined over pal_file_read.
func TestReadBytesBody(t *testing.T) {
	ir := generateIR(t, `
		main!() {
			u8[] buf = Vector[u8].filled(0u8, 8);
			int n = stdin.read(buf)?^;
		}
	`)
	assertContains(t, ir, "define void @promise_read_bytes(i8* %sret, i8* %fd, i8* %buf)")
	assertContainsMatch(t, ir, `@promise_read_bytes\(i8\* %sret.*\n(.*\n)*?.*call i64 @pal_file_read\(i32 %`)
}

// TestStderrGetterUsesFd2 verifies the stderr getter bakes in fd 2 and boxes the
// private value type into a Writer fat pointer (value->structural conversion).
func TestStderrGetterUsesFd2(t *testing.T) {
	ir := generateIR(t, `
		main!() { stderr.write_line("x")?^; }
	`)
	// _OutputStream is a pure value type: {vtable_ptr, i64 fd}.
	assertContains(t, ir, "%promise__OutputStream_v = type { i8*, i64 }")
	// The stderr getter stores fd 2 into the value.
	assertContains(t, ir, "define { i8*, i8* } @__mod_std_stderr()")
	assertContains(t, ir, "insertvalue %promise__OutputStream_v %0, i64 2, 1")
	// Boxed to the Writer view (structural interface fat pointer).
	assertContains(t, ir, "@promise_vtable__OutputStream_as_Writer")
}

// TestStdoutGetterUsesFd1 verifies the stdout getter bakes in fd 1.
func TestStdoutGetterUsesFd1(t *testing.T) {
	ir := generateIR(t, `
		main!() { stdout.write_line("y")?^; }
	`)
	assertContains(t, ir, "define { i8*, i8* } @__mod_std_stdout()")
	assertContains(t, ir, "insertvalue %promise__OutputStream_v %0, i64 1, 1")
}

// TestStdinGetterUsesFd0 verifies the stdin getter bakes in fd 0 and returns a
// Reader view.
func TestStdinGetterUsesFd0(t *testing.T) {
	ir := generateIR(t, `
		main!() {
			u8[] buf = Vector[u8].filled(0u8, 8);
			int n = stdin.read(buf)?^;
		}
	`)
	assertContains(t, ir, "%promise__InputStream_v = type { i8*, i64 }")
	assertContains(t, ir, "define { i8*, i8* } @__mod_std_stdin()")
	assertContains(t, ir, "insertvalue %promise__InputStream_v %0, i64 0, 1")
	assertContains(t, ir, "@promise_vtable__InputStream_as_Reader")
}

// TestPrintStringStillFd1 is a regression guard: the untouched print path still
// writes to fd 1 through promise_print_string.
func TestPrintStringStillFd1(t *testing.T) {
	ir := generateIR(t, `
		main() { print_line("hi"); }
	`)
	assertContains(t, ir, "define void @promise_print_string(i8* %s)")
	assertContains(t, ir, "call i64 @pal_write(i32 1,") // stdout
}
