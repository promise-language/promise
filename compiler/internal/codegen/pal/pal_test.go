package pal

import (
	"strings"
	"testing"

	"github.com/llir/llvm/ir"
)

func TestPosixPALEmitWrite(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{}
	fn := p.EmitWrite(module)

	out := module.String()

	// libc write declaration
	if !strings.Contains(out, "declare i64 @write(i32 %fd, i8* %buf, i64 %len)") {
		t.Error("missing @write declaration")
	}
	// pal_write wrapper definition
	if !strings.Contains(out, "define i64 @pal_write(i32 %fd, i8* %buf, i64 %len)") {
		t.Error("missing @pal_write definition")
	}
	// wrapper calls through to write
	if !strings.Contains(out, "call i64 @write(i32 %fd, i8* %buf, i64 %len)") {
		t.Error("missing call to @write in pal_write body")
	}
	// verify return
	if fn.Name() != "pal_write" {
		t.Errorf("expected function name pal_write, got %s", fn.Name())
	}
}

func TestPosixPALEmitExit(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{}
	fn := p.EmitExit(module)

	out := module.String()

	// libc exit declaration with noreturn
	if !strings.Contains(out, "@exit(i32 %code)") {
		t.Error("missing @exit declaration")
	}
	// pal_exit wrapper definition
	if !strings.Contains(out, "define void @pal_exit(i32 %code)") {
		t.Error("missing @pal_exit definition")
	}
	// wrapper calls exit and is unreachable
	if !strings.Contains(out, "call void @exit(i32 %code)") {
		t.Error("missing call to @exit in pal_exit body")
	}
	if !strings.Contains(out, "unreachable") {
		t.Error("missing unreachable in pal_exit body")
	}
	if fn.Name() != "pal_exit" {
		t.Errorf("expected function name pal_exit, got %s", fn.Name())
	}
}

// --- Windows PAL Tests ---

func TestWindowsPALEmitWrite(t *testing.T) {
	module := ir.NewModule()
	p := &WindowsPAL{}
	fn := p.EmitWrite(module)

	out := module.String()

	// Win32 GetStdHandle declaration
	if !strings.Contains(out, "@GetStdHandle(i32 %nStdHandle)") {
		t.Error("missing @GetStdHandle declaration")
	}
	// Win32 WriteFile declaration
	if !strings.Contains(out, "@WriteFile(") {
		t.Error("missing @WriteFile declaration")
	}
	// pal_write wrapper definition
	if !strings.Contains(out, "define i64 @pal_write(i32 %fd, i8* %buf, i64 %len)") {
		t.Error("missing @pal_write definition")
	}
	// fd-to-handle mapping: sub i32 -10, %fd
	if !strings.Contains(out, "sub i32 -10, %fd") {
		t.Error("missing fd-to-handle mapping (sub i32 -10, fd)")

	}
	// Call to GetStdHandle
	if !strings.Contains(out, "call i8* @GetStdHandle(") {
		t.Error("missing call to @GetStdHandle")
	}
	// Truncation of len to i32
	if !strings.Contains(out, "trunc i64 %len to i32") {
		t.Error("missing trunc of len to i32")
	}
	// Alloca for bytes written
	if !strings.Contains(out, "alloca i32") {
		t.Error("missing alloca i32 for bytes written")
	}
	// Call to WriteFile
	if !strings.Contains(out, "call i32 @WriteFile(") {
		t.Error("missing call to @WriteFile")
	}
	// Null overlapped pointer passed to WriteFile
	if !strings.Contains(out, "null") {
		t.Error("missing null pointer for lpOverlapped")
	}
	// Load written count from alloca before zext
	if !strings.Contains(out, "load i32, i32*") {
		t.Error("missing load of written bytes from alloca")
	}
	// Zero-extend written to i64
	if !strings.Contains(out, "zext i32") {
		t.Error("missing zext of written bytes to i64")
	}
	if fn.Name() != "pal_write" {
		t.Errorf("expected function name pal_write, got %s", fn.Name())
	}
}

func TestWindowsPALEmitExit(t *testing.T) {
	module := ir.NewModule()
	p := &WindowsPAL{}
	fn := p.EmitExit(module)

	out := module.String()

	// Win32 ExitProcess declaration with noreturn
	if !strings.Contains(out, "@ExitProcess(i32 %uExitCode)") {
		t.Error("missing @ExitProcess declaration")
	}
	// noreturn attribute on pal_exit wrapper
	if !strings.Contains(out, "noreturn") {
		t.Error("missing noreturn attribute on pal_exit")
	}
	// pal_exit wrapper definition
	if !strings.Contains(out, "define void @pal_exit(i32 %code)") {
		t.Error("missing @pal_exit definition")
	}
	// wrapper calls ExitProcess and is unreachable
	if !strings.Contains(out, "call void @ExitProcess(i32 %code)") {
		t.Error("missing call to @ExitProcess in pal_exit body")
	}
	if !strings.Contains(out, "unreachable") {
		t.Error("missing unreachable in pal_exit body")
	}
	if fn.Name() != "pal_exit" {
		t.Errorf("expected function name pal_exit, got %s", fn.Name())
	}
}

// --- WASM PAL Tests ---

func TestWasmPALEmitWrite(t *testing.T) {
	module := ir.NewModule()
	p := &WasmPAL{}
	fn := p.EmitWrite(module)

	out := module.String()

	// WASI fd_write declaration
	if !strings.Contains(out, "@fd_write(") {
		t.Error("missing @fd_write declaration")
	}
	// pal_write wrapper definition
	if !strings.Contains(out, "define i64 @pal_write(i32 %fd, i8* %buf, i64 %len)") {
		t.Error("missing @pal_write definition")
	}
	// iovec alloca (anonymous struct {i8*, i32})
	if !strings.Contains(out, "alloca { i8*, i32 }") {
		t.Error("missing iovec alloca")
	}
	// GEP into iovec fields
	if !strings.Contains(out, "getelementptr { i8*, i32 }") {
		t.Error("missing GEP into iovec struct")
	}
	// Truncation of len to i32 for iovec
	if !strings.Contains(out, "trunc i64 %len to i32") {
		t.Error("missing trunc of len to i32 for iovec")
	}
	// At least 2 alloca instructions (iovec + nwritten)
	if strings.Count(out, "alloca") < 2 {
		t.Error("expected at least 2 alloca instructions (iovec + nwritten)")
	}
	// Store instructions for iovec fields (buf pointer + length)
	if strings.Count(out, "store") < 2 {
		t.Error("expected at least 2 store instructions (buf ptr + len into iovec)")
	}
	// Call to fd_write with iovs_len=1
	if !strings.Contains(out, "call i32 @fd_write(") {
		t.Error("missing call to @fd_write")
	}
	// Load nwritten from alloca before zext
	if !strings.Contains(out, "load i32, i32*") {
		t.Error("missing load of nwritten from alloca")
	}
	// Zero-extend nwritten to i64
	if !strings.Contains(out, "zext i32") {
		t.Error("missing zext of nwritten to i64")
	}
	if fn.Name() != "pal_write" {
		t.Errorf("expected function name pal_write, got %s", fn.Name())
	}
}

func TestWasmPALEmitExit(t *testing.T) {
	module := ir.NewModule()
	p := &WasmPAL{}
	fn := p.EmitExit(module)

	out := module.String()

	// WASI proc_exit declaration
	if !strings.Contains(out, "@proc_exit(i32 %rval)") {
		t.Error("missing @proc_exit declaration")
	}
	// noreturn attribute on pal_exit wrapper
	if !strings.Contains(out, "noreturn") {
		t.Error("missing noreturn attribute on pal_exit")
	}
	// pal_exit wrapper definition
	if !strings.Contains(out, "define void @pal_exit(i32 %code)") {
		t.Error("missing @pal_exit definition")
	}
	// wrapper calls proc_exit and is unreachable
	if !strings.Contains(out, "call void @proc_exit(i32 %code)") {
		t.Error("missing call to @proc_exit in pal_exit body")
	}
	if !strings.Contains(out, "unreachable") {
		t.Error("missing unreachable in pal_exit body")
	}
	if fn.Name() != "pal_exit" {
		t.Errorf("expected function name pal_exit, got %s", fn.Name())
	}
}

// --- Allocator tests (shared libc wrappers, tested across all PALs) ---

func TestEmitAlloc(t *testing.T) {
	pals := []struct {
		name string
		pal  PAL
	}{
		{"Posix", &PosixPAL{}},
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	}
	for _, tc := range pals {
		t.Run(tc.name, func(t *testing.T) {
			module := ir.NewModule()
			fn := tc.pal.EmitAlloc(module)
			out := module.String()

			if !strings.Contains(out, "@malloc(i64") {
				t.Error("missing @malloc declaration")
			}
			if !strings.Contains(out, "noalias") {
				t.Error("missing noalias attribute")
			}
			if !strings.Contains(out, "noundef") {
				t.Error("missing noundef attribute on malloc size param")
			}
			if !strings.Contains(out, "define noalias i8* @pal_alloc(i64 %size)") {
				t.Error("missing @pal_alloc definition with noalias return")
			}
			if !strings.Contains(out, "call i8* @malloc(i64 %size)") {
				t.Error("missing call to @malloc in pal_alloc body")
			}
			if !strings.Contains(out, "nounwind") {
				t.Error("missing nounwind attribute")
			}
			if !strings.Contains(out, "willreturn") {
				t.Error("missing willreturn attribute")
			}
			if fn.Name() != "pal_alloc" {
				t.Errorf("expected function name pal_alloc, got %s", fn.Name())
			}
		})
	}
}

func TestEmitFree(t *testing.T) {
	pals := []struct {
		name string
		pal  PAL
	}{
		{"Posix", &PosixPAL{}},
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	}
	for _, tc := range pals {
		t.Run(tc.name, func(t *testing.T) {
			module := ir.NewModule()
			fn := tc.pal.EmitFree(module)
			out := module.String()

			if !strings.Contains(out, "@free(i8*") {
				t.Error("missing @free declaration")
			}
			if !strings.Contains(out, "nocapture") {
				t.Error("missing nocapture attribute on @free param")
			}
			if !strings.Contains(out, "noundef") {
				t.Error("missing noundef attribute on @free param")
			}
			if !strings.Contains(out, "define void @pal_free(i8* %ptr)") {
				t.Error("missing @pal_free definition")
			}
			if !strings.Contains(out, "call void @free(i8* %ptr)") {
				t.Error("missing call to @free in pal_free body")
			}
			if !strings.Contains(out, "nounwind") {
				t.Error("missing nounwind attribute on pal_free")
			}
			if !strings.Contains(out, "willreturn") {
				t.Error("missing willreturn attribute on pal_free")
			}
			if fn.Name() != "pal_free" {
				t.Errorf("expected function name pal_free, got %s", fn.Name())
			}
		})
	}
}

func TestEmitRealloc(t *testing.T) {
	pals := []struct {
		name string
		pal  PAL
	}{
		{"Posix", &PosixPAL{}},
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	}
	for _, tc := range pals {
		t.Run(tc.name, func(t *testing.T) {
			module := ir.NewModule()
			fn := tc.pal.EmitRealloc(module)
			out := module.String()

			if !strings.Contains(out, "@realloc(i8*") {
				t.Error("missing @realloc declaration")
			}
			if !strings.Contains(out, "noalias") {
				t.Error("missing noalias attribute")
			}
			if !strings.Contains(out, "nocapture") {
				t.Error("missing nocapture attribute on realloc ptr param")
			}
			if !strings.Contains(out, "noundef") {
				t.Error("missing noundef attribute on realloc params")
			}
			if !strings.Contains(out, "define noalias i8* @pal_realloc(i8* %ptr, i64 %size)") {
				t.Error("missing @pal_realloc definition with noalias return")
			}
			if !strings.Contains(out, "call i8* @realloc(i8* %ptr, i64 %size)") {
				t.Error("missing call to @realloc in pal_realloc body")
			}
			if !strings.Contains(out, "nounwind") {
				t.Error("missing nounwind attribute on pal_realloc")
			}
			if !strings.Contains(out, "willreturn") {
				t.Error("missing willreturn attribute on pal_realloc")
			}
			if fn.Name() != "pal_realloc" {
				t.Errorf("expected function name pal_realloc, got %s", fn.Name())
			}
		})
	}
}

// --- ForTarget dispatch tests ---

func TestForTargetReturnsPosixPAL(t *testing.T) {
	triples := []string{
		"arm64-apple-macosx11.0.0",
		"x86_64-apple-macosx10.15.0",
		"aarch64-unknown-linux-gnu",
		"x86_64-unknown-linux-gnu",
	}
	for _, triple := range triples {
		p := ForTarget(triple)
		if _, ok := p.(*PosixPAL); !ok {
			t.Errorf("ForTarget(%q) returned %T, expected *PosixPAL", triple, p)
		}
	}
}

func TestForTargetReturnsWindowsPAL(t *testing.T) {
	triples := []string{
		"x86_64-pc-windows-msvc",
		"aarch64-pc-windows-msvc",
		"x86_64-unknown-windows-gnu",
	}
	for _, triple := range triples {
		p := ForTarget(triple)
		if _, ok := p.(*WindowsPAL); !ok {
			t.Errorf("ForTarget(%q) returned %T, expected *WindowsPAL", triple, p)
		}
	}
}

func TestForTargetReturnsWasmPAL(t *testing.T) {
	triples := []string{
		"wasm32-unknown-wasi",
		"wasm32-unknown-unknown",
		"wasm64-unknown-wasi",
	}
	for _, triple := range triples {
		p := ForTarget(triple)
		if _, ok := p.(*WasmPAL); !ok {
			t.Errorf("ForTarget(%q) returned %T, expected *WasmPAL", triple, p)
		}
	}
}
